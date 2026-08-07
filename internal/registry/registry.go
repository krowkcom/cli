// Package registry is an in-memory stand-in for api.krowk.com, so the CLI can
// be tested and demoed without Postgres, object storage or a Rails process.
//
// It implements the same contract the real registry does — declare, upload,
// finalize; runs; the claim flow; one error envelope — including the parts that
// exist to catch a broken client: it refuses a finalize for bytes that never
// arrived, and refuses bytes whose length or digest is not what was declared.
// A client that passes against this one is exercising the real sequence.
//
// The one thing it fakes is signing. Object storage is this same process on a
// /_storage path, and the "presigned" URL carries an opaque token rather than a
// SigV4 signature. Everything the CLI has to get right — the exact headers, the
// declared length, the digest — is still checked.
package registry

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultLimitBytes matches the registry's own max_upload_bytes.
	DefaultLimitBytes int64 = 100 << 20

	// How long a keyless upload survives, matching Artifact::EPHEMERAL_LIFETIME.
	ephemeralLifetime = 24 * time.Hour

	uploadURLLifetime = 15 * time.Minute

	// How many artifacts a page holds, mirroring the registry's own bounds. The
	// caller picks, so the ceiling is enforced rather than trusted.
	defaultPageSize = 50
	maxPageSize     = 100

	// The workspace slug keyless uploads land in. One shared workspace, as in
	// the real registry, so the storage keys look the same.
	anonymousWorkspace = "ws_anonymous00000000"
)

type artifact struct {
	Slug        string `json:"slug"`
	State       string `json:"state"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	ByteSize    int64  `json:"byte_size"`
	Checksum    string `json:"checksum,omitempty"`
	Region      string `json:"region"`
	Run         any    `json:"run"`
	URL         string `json:"url"`
	Markdown    string `json:"markdown"`
	ExpiresAt   any    `json:"expires_at"`
	CreatedAt   string `json:"created_at"`

	// Not serialized: what the stand-in has to remember between calls.
	workspace  string
	claimHash  string
	uploaded   bool
	storedSize int64
	storedSum  string
	uploadTok  string
	uploadTil  time.Time
	claimed    bool
	// deletedAt turns the record into a tombstone: the bytes are gone but the row
	// stays, so a slug that was taken down answers 410 rather than 404. Zero means
	// it is still live.
	deletedAt time.Time
	// storageKey is pinned at declare time: claiming moves the artifact to a
	// new workspace, but the bytes stay where the presigned URL was signed for.
	storageKey string
	// seq orders a listing. A timestamp would tie when two artifacts are created
	// in the same instant, and a page has to be totally ordered or rows swap
	// places between pages.
	seq int
}

type run struct {
	Slug       string          `json:"slug"`
	Status     string          `json:"status"`
	StartedAt  string          `json:"started_at"`
	FinishedAt any             `json:"finished_at"`
	Metadata   json.RawMessage `json:"metadata"`
	CreatedAt  string          `json:"created_at"`

	workspace string
	// seq orders a listing, for the same reason an artifact's does: a timestamp
	// would tie two runs opened in the same instant, and a page has to be totally
	// ordered or rows swap places between pages.
	seq int
}

// answered is a create this stand-in has already replied to, found again by the
// Idempotency-Key a client named its attempt with.
//
// requestHash is what the key was first used for, because the same key arriving
// with a different payload is a refusal rather than a replay. The record is held
// by slug rather than by pointer so this cannot end up pointing at an artifact
// that is gone.
type answered struct {
	requestHash string
	artifact    string
	run         string
}

type store struct {
	mu        sync.Mutex
	artifacts map[string]*artifact
	runs      map[string]*run
	objects   map[string][]byte
	// Keyed by what is being created, who is creating it, and the client's key —
	// all three, as the real registry digests all three. Per kind so one key can
	// cover the run and the artifact of a single push; per caller so one client's
	// key cannot collide with, or be replayed by, another's.
	idempotent map[string]*answered
	created    int
	runsOpen   int
	now        func() time.Time
}

// Handler serves the stand-in registry. siteURL is the origin baked into the
// links it hands out; empty means "whatever host the request arrived on".
// limitBytes caps an upload the way the real registry's max_upload_bytes does.
func Handler(limitBytes int64, siteURL string) http.Handler {
	return HandlerWithClock(limitBytes, siteURL, time.Now)
}

// HandlerWithClock is Handler with the clock injected, which is the only way a
// test can reach the expiry surface: a 24-hour lifetime and a 15-minute upload
// window are not going to elapse inside one.
func HandlerWithClock(limitBytes int64, siteURL string, now func() time.Time) http.Handler {
	if limitBytes <= 0 {
		limitBytes = DefaultLimitBytes
	}
	s := &store{
		artifacts:  map[string]*artifact{},
		runs:       map[string]*run{},
		objects:    map[string][]byte{},
		idempotent: map[string]*answered{},
		now:        now,
	}

	mux := http.NewServeMux()

	// The service descriptor, which is what a reachability check reads.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"service": "krowk-registry", "versions": []string{"v1"},
		})
	})

	mux.HandleFunc("POST /v1/artifacts", func(w http.ResponseWriter, r *http.Request) {
		s.createArtifact(w, r, limitBytes, site(r, siteURL))
	})
	mux.HandleFunc("GET /v1/artifacts", s.listArtifacts)
	mux.HandleFunc("GET /v1/artifacts/{slug}", s.showArtifact)

	// Takedown, and a hard purge: the bytes go at once rather than into anything
	// that could hand them back.
	mux.HandleFunc("DELETE /v1/artifacts/{slug}", s.destroyArtifact)

	// Finalizing and completing are nested resources reached with PUT, because
	// they are idempotent; claiming spends a one-shot token, so it is a POST.
	// PATCH is accepted alongside PUT, as the registry's routes do.
	mux.HandleFunc("PUT /v1/artifacts/{slug}/finalization", s.finalizeArtifact)
	mux.HandleFunc("PATCH /v1/artifacts/{slug}/finalization", s.finalizeArtifact)
	mux.HandleFunc("POST /v1/artifacts/{slug}/claim", s.claimArtifact)

	// The run an artifact belongs to is a singular nested resource, so it is set
	// with a PUT rather than posted to: an artifact ends up under the same run
	// however many times it is asked for.
	mux.HandleFunc("PUT /v1/artifacts/{slug}/run", s.attachRun)
	mux.HandleFunc("PATCH /v1/artifacts/{slug}/run", s.attachRun)

	// The key the request is made with — a singular resource, read with a GET.
	mux.HandleFunc("GET /v1/key", showKey)

	mux.HandleFunc("POST /v1/runs", s.createRun)
	mux.HandleFunc("GET /v1/runs", s.listRuns)
	mux.HandleFunc("GET /v1/runs/{slug}", s.showRun)
	mux.HandleFunc("PUT /v1/runs/{slug}/completion", s.finishRun)
	mux.HandleFunc("PATCH /v1/runs/{slug}/completion", s.finishRun)

	// What one run produced, served as a collection of the run rather than as a
	// filter on the artifact listing: the run is looked up first, so an unknown
	// slug is a 404 where a filter would answer an empty page — which a client
	// cannot tell apart from a run that genuinely produced nothing.
	mux.HandleFunc("GET /v1/runs/{slug}/artifacts", s.listRunArtifacts)

	// Object storage, standing in for R2. PUT is the presigned upload target;
	// GET is the CDN, so a link this stand-in hands out actually resolves.
	mux.HandleFunc("PUT /_storage/{workspace}/{slug}/{filename}", s.putObject)
	mux.HandleFunc("GET /_storage/{workspace}/{slug}/{filename}", s.getObject)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "No such endpoint.", nil)
	})

	return mux
}

// createArtifact records the artifact and hands back where to put the bytes.
func (s *store) createArtifact(w http.ResponseWriter, r *http.Request, limitBytes int64, site string) {
	workspace, ok := authenticate(w, r)
	if !ok {
		return
	}

	var body struct {
		Artifact struct {
			Filename    string `json:"filename"`
			ContentType string `json:"content_type"`
			ByteSize    int64  `json:"byte_size"`
			Checksum    string `json:"checksum"`
			Run         string `json:"run"`
		} `json:"artifact"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "parameter_missing",
			"Missing required parameter: artifact.", nil)
		return
	}
	in := body.Artifact

	switch {
	case in.Filename == "":
		writeError(w, http.StatusUnprocessableEntity, "invalid", "Filename can't be blank",
			map[string]any{"filename": []string{"can't be blank"}})
		return
	case in.ContentType == "":
		writeError(w, http.StatusUnprocessableEntity, "invalid", "Content type can't be blank",
			map[string]any{"content_type": []string{"can't be blank"}})
		return
	case in.ByteSize <= 0:
		writeError(w, http.StatusUnprocessableEntity, "invalid",
			"Byte size must be greater than 0",
			map[string]any{"byte_size": []string{"must be greater than 0"}})
		return
	case in.ByteSize > limitBytes:
		writeError(w, http.StatusUnprocessableEntity, "invalid",
			fmt.Sprintf("Byte size must be at most %d bytes", limitBytes),
			map[string]any{"byte_size": []string{fmt.Sprintf("must be at most %d bytes", limitBytes)}})
		return
	}

	anonymous := workspace == ""
	if anonymous {
		workspace = anonymousWorkspace
	}

	// A keyless upload naming a run is refused rather than quietly ignored:
	// answering 201 for an upload not attached to the run the client asked for
	// looks like success and is not.
	if in.Run != "" && anonymous {
		writeError(w, http.StatusUnprocessableEntity, "run_needs_key",
			"Attaching an artifact to a run needs an API key — a keyless upload has no workspace.", nil)
		return
	}

	attempt, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	// Who the key belongs to. A keyed request has a workspace the token proves; a
	// keyless one has nothing to prove anything with, so it falls back to the
	// address it came from — which makes the key itself the credential for a
	// keyless caller.
	scope := workspace
	if anonymous {
		scope = "ip:" + clientAddress(r)
	}
	requestHash := sha256Hex([]byte(fmt.Sprintf("%q", []string{
		in.Filename, in.ContentType, itoa(in.ByteSize), strings.ToLower(in.Checksum), in.Run,
	})))

	s.mu.Lock()
	defer s.mu.Unlock()

	if attempt != "" {
		if found, matches := s.replay("artifact", scope, attempt, requestHash); found != nil {
			if s.replayDeclare(w, found, matches) {
				return
			}
		}
	}

	if in.Run != "" {
		if existing, ok := s.runs[in.Run]; !ok || existing.workspace != workspace {
			writeError(w, http.StatusNotFound, "not_found", "No such record.", nil)
			return
		}
	}

	slug := generateSlug("art")
	key := path.Join(workspace, slug, safeFilename(in.Filename))
	url := site + "/_storage/" + key
	now := s.now().UTC()

	a := &artifact{
		Slug:        slug,
		State:       "pending",
		Filename:    in.Filename,
		ContentType: in.ContentType,
		ByteSize:    in.ByteSize,
		Checksum:    strings.ToLower(in.Checksum),
		Region:      "weur",
		URL:         url,
		Markdown:    markdown(in.Filename, in.ContentType, url),
		CreatedAt:   now.Format(time.RFC3339Nano),
		workspace:   workspace,
		uploadTok:   randomToken(),
		uploadTil:   now.Add(uploadURLLifetime),
		storageKey:  key,
	}
	if in.Run != "" {
		a.Run = in.Run
	}
	s.created++
	a.seq = s.created

	var claimToken string
	if anonymous {
		a.ExpiresAt = now.Add(ephemeralLifetime).Format(time.RFC3339Nano)
		claimToken = "krowk_claim_" + randomToken()
		a.claimHash = sha256Hex([]byte(claimToken))
	}
	s.artifacts[slug] = a

	if attempt != "" {
		s.remember("artifact", scope, attempt, requestHash, &answered{artifact: slug})
	}

	writeJSON(w, http.StatusCreated, s.declared(a, claimToken))
}

// declared is what a create answers with: the artifact, where to put its bytes,
// and what to do next.
//
// Split out because a replay answers the same shape over the record it already
// made. The upload block is minted again there rather than repeated from the first
// answer — a signature is good for 15 minutes while an artifact waits far longer,
// so a stored one is usually dead by the time a retry asks.
func (s *store) declared(a *artifact, claimToken string) map[string]any {
	payload := map[string]any{}
	for k, v := range serializeArtifact(a) {
		payload[k] = v
	}

	uploadHeaders := map[string]string{"Content-Type": a.ContentType, "Content-Length": itoa(a.ByteSize)}
	// Handed over because the real presign signs it as a header: storage reads the
	// checksum from there, not from the query string, and refuses the PUT without
	// it. A client that sends the headers it is given needs no change for it.
	if a.Checksum != "" {
		uploadHeaders["x-amz-checksum-sha256"] = base64Sum(a.Checksum)
	}
	payload["upload"] = map[string]any{
		"method":     "PUT",
		"url":        a.URL + "?upload_token=" + a.uploadTok,
		"headers":    uploadHeaders,
		"expires_at": a.uploadTil.Format(time.RFC3339Nano),
	}
	payload["next_step"] = "PUT the file to upload.url with the headers in upload.headers, " +
		"then PUT /v1/artifacts/" + a.Slug + "/finalization"
	if claimToken != "" {
		payload["claim_token"] = claimToken
	}
	return payload
}

// replayDeclare answers a retried declare with the artifact the first attempt
// made, and reports whether it handled the request.
//
// The state is asked about before a URL is handed out, because a key outlives its
// artifact's lifecycle: takedown keeps the row, so without this a replay would
// mint a fresh PUT over the storage key of an artifact somebody deleted. Ready and
// expired are refused for the same reason the represign endpoint refuses them.
//
// One place this stand-in is deliberately thinner than the real registry: a real
// presign leaves earlier signatures valid until they expire, while re-minting here
// revokes the previous token. Nothing a client does depends on the old one — the
// retry uses the URL it was just handed — so the simpler model stays.
func (s *store) replayDeclare(w http.ResponseWriter, found *answered, matches bool) bool {
	a, ok := s.artifacts[found.artifact]
	if !ok {
		return false
	}
	if !matches {
		keyReused(w, a.Slug)
		return true
	}
	if s.refuseIfGone(w, a) {
		return true
	}
	if a.State == "ready" {
		writeError(w, http.StatusConflict, "already_finalized",
			a.Slug+" is already finalized — declare a new artifact for new bytes", nil)
		return true
	}

	now := s.now().UTC()
	a.uploadTok = randomToken()
	a.uploadTil = now.Add(uploadURLLifetime)

	// The claim token is not re-issued. Re-minting one would void the token the
	// first response may already have delivered, and the row keeps only its digest
	// regardless — as in the real registry.
	writeJSON(w, http.StatusCreated, s.declared(a, ""))
	return true
}

// putObject is object storage. It enforces what the real presigned URL enforces
// through its signature: the token, the declared length and the declared digest.
func (s *store) putObject(w http.ResponseWriter, r *http.Request) {
	key := path.Join(r.PathValue("workspace"), r.PathValue("slug"), r.PathValue("filename"))

	// Everything the checks need is copied under the lock: finalizeArtifact
	// mutates ByteSize and Checksum, and a PUT racing a finalize must not read
	// them bare.
	s.mu.Lock()
	a := s.artifacts[r.PathValue("slug")]
	var wantKey, wantTok, wantType, wantSum string
	var wantSize int64
	var until time.Time
	if a != nil {
		wantKey = a.storageKey
		wantTok, wantType, wantSum = a.uploadTok, a.ContentType, a.Checksum
		wantSize, until = a.ByteSize, a.uploadTil
	}
	s.mu.Unlock()

	// A real presigned URL signs the key, not just the object: bytes must land
	// exactly where the artifact says they live, never under a rewritten
	// filename or another workspace's prefix.
	if a == nil || key != wantKey || wantTok == "" || r.URL.Query().Get("upload_token") != wantTok {
		// Storage speaks XML, as R2 and S3 do, so a client cannot get away with
		// assuming every failure is a krowk envelope. A finalized artifact's token
		// is spent, so a re-PUT of a ready permalink lands here too.
		writeXMLError(w, http.StatusForbidden, "SignatureDoesNotMatch")
		return
	}
	// The URL's advertised expiry is enforced, as real object storage enforces
	// the signature's window.
	if s.now().After(until) {
		writeXMLError(w, http.StatusForbidden, "AccessDenied")
		return
	}
	if got := r.Header.Get("Content-Type"); got != wantType {
		writeXMLError(w, http.StatusForbidden, "SignatureDoesNotMatch")
		return
	}
	// The digest is signed as a header, so real storage refuses the PUT before it
	// reads the body when that header is missing or altered — the signature does
	// not verify without it. A client that ignores upload.headers has to fail here
	// too, or it passes against this and fails against R2.
	if wantSum != "" && r.Header.Get("x-amz-checksum-sha256") != base64Sum(wantSum) {
		writeXMLError(w, http.StatusForbidden, "SignatureDoesNotMatch")
		return
	}

	bytes, err := io.ReadAll(io.LimitReader(r.Body, wantSize+1))
	if err != nil {
		writeXMLError(w, http.StatusBadRequest, "IncompleteBody")
		return
	}
	// The length is signed in, so storage refuses a body of any other size.
	if int64(len(bytes)) != wantSize {
		writeXMLError(w, http.StatusBadRequest, "IncorrectContentLength")
		return
	}
	// So is the digest, when one was declared — corruption is caught at the edge
	// rather than stored and discovered later.
	sum := sha256Hex(bytes)
	if wantSum != "" && sum != wantSum {
		writeXMLError(w, http.StatusBadRequest, "BadDigest")
		return
	}

	s.mu.Lock()
	// A finalize may have landed while the body was being read; a ready
	// artifact's bytes are immutable.
	if a.uploadTok != wantTok {
		s.mu.Unlock()
		writeXMLError(w, http.StatusForbidden, "SignatureDoesNotMatch")
		return
	}
	s.objects[key] = bytes
	a.uploaded = true
	a.storedSize = int64(len(bytes))
	a.storedSum = sum
	s.mu.Unlock()

	w.WriteHeader(http.StatusOK)
}

func (s *store) getObject(w http.ResponseWriter, r *http.Request) {
	key := path.Join(r.PathValue("workspace"), r.PathValue("slug"), r.PathValue("filename"))

	s.mu.Lock()
	bytes, ok := s.objects[key]
	// The 24-hour promise covers the bytes, not just the record: an expired
	// artifact's public URL stops serving, as the real registry's lifecycle
	// rule deletes the object. Gone reads as never-there, the way storage does.
	if a := s.artifacts[r.PathValue("slug")]; a != nil && s.expired(a) {
		ok = false
	}
	s.mu.Unlock()

	if !ok {
		writeXMLError(w, http.StatusNotFound, "NoSuchKey")
		return
	}
	_, _ = w.Write(bytes)
}

// finalizeArtifact verifies what landed and marks the artifact ready. Idempotent,
// because agents retry.
func (s *store) finalizeArtifact(w http.ResponseWriter, r *http.Request) {
	workspace, ok := authenticate(w, r)
	if !ok {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	a := s.find(workspace, r.PathValue("slug"))
	if a == nil {
		writeError(w, http.StatusNotFound, "not_found", "No such record.", nil)
		return
	}
	// Gone is checked before the idempotent success, because a taken-down artifact
	// is normally a ready one: answering 200 for it would hand back a url and a
	// markdown snippet pointing at bytes that are no longer there.
	if s.refuseIfGone(w, a) {
		return
	}
	if a.State == "ready" {
		writeJSON(w, http.StatusOK, serializeArtifact(a))
		return
	}
	// 409 rather than 422: the request is well formed, it just arrived before the
	// upload it describes, so retrying after uploading is the fix.
	if !a.uploaded {
		writeError(w, http.StatusConflict, "upload_missing",
			"nothing uploaded for "+a.Slug+" yet", nil)
		return
	}
	if a.storedSize == 0 {
		writeError(w, http.StatusUnprocessableEntity, "empty_upload",
			"what was uploaded for "+a.Slug+" is empty", nil)
		return
	}
	if a.Checksum != "" && a.storedSum != a.Checksum {
		writeError(w, http.StatusUnprocessableEntity, "checksum_mismatch",
			fmt.Sprintf("%s was declared as %s but storage holds %s", a.Slug, a.Checksum, a.storedSum), nil)
		return
	}

	// The size on the record becomes what storage actually holds, not what the
	// client claimed it would send. The upload token is spent: a ready
	// artifact's bytes are immutable, so its presigned URL stops working.
	a.State = "ready"
	a.ByteSize = a.storedSize
	if a.Checksum == "" {
		a.Checksum = a.storedSum
	}
	a.uploadTok = ""
	writeJSON(w, http.StatusOK, serializeArtifact(a))
}

// listArtifacts is one page of a workspace's artifacts, newest first. It needs a
// key: keyless requests all share the anonymous workspace, so listing it would
// show every anonymous upload anyone has ever made.
//
// The page is "older than this one" rather than an offset, so rows are neither
// skipped nor repeated when something is uploaded mid-listing.
func (s *store) listArtifacts(w http.ResponseWriter, r *http.Request) {
	workspace, ok := requireKey(w, r)
	if !ok {
		return
	}

	limit := pageLimit(r)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Tombstones are not part of what a workspace holds: a tombstone records that
	// something used to be at a slug, not something still there, so it belongs in
	// the answer to "what is at this slug" and nowhere else.
	var owned []*artifact
	for _, a := range s.artifacts {
		if a.workspace == workspace && a.deletedAt.IsZero() {
			owned = append(owned, a)
		}
	}
	slices.SortFunc(owned, func(x, y *artifact) int { return y.seq - x.seq })

	if before := r.URL.Query().Get("before"); before != "" {
		// Looked up in the same workspace as the page, so a slug from another
		// tenant reads as simply not existing.
		cursor := s.find(workspace, before)
		if cursor == nil {
			writeError(w, http.StatusNotFound, "not_found", "No such record.", nil)
			return
		}
		owned = slices.DeleteFunc(owned, func(a *artifact) bool { return a.seq >= cursor.seq })
	}

	owned, next := paginate(owned, limit, func(a *artifact) string { return a.Slug })

	page := make([]map[string]any, 0, len(owned))
	for _, a := range owned {
		page = append(page, serializeArtifact(a))
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": page, "next": next})
}

// pageLimit is how many rows a listing serves, clamped rather than refused: a
// caller asking for more than we serve gets the most we serve, which is what it
// wanted. Anything that is not a number at all is not a request for one, so it
// gets the default rather than being read as zero.
func pageLimit(r *http.Request) int {
	requested, err := strconv.Atoi(r.URL.Query().Get("limit"))
	// A number too large to hold is still a number asked for. Ruby has no such
	// ceiling, so the registry parses it and clamps it like any other — and Atoi
	// saturates rather than losing it, reporting ErrRange, so clamping the
	// saturated value lands in the same place. Only something that is not a
	// number at all takes the default.
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return defaultPageSize
	}
	return min(max(requested, 1), maxPageSize)
}

// paginate cuts a newest-first slice down to one page and names the cursor for
// the next one.
//
// next is present whenever the page came back full, as the registry's own
// listings are — so a total that is an exact multiple of the limit costs one
// extra empty page rather than a count query on every listing. Shared by every
// listing here, because a client that learned the rule from one of them has to
// be right about the next.
func paginate[T any](rows []T, limit int, slug func(T) string) ([]T, any) {
	if len(rows) > limit {
		rows = rows[:limit]
	}
	if len(rows) < limit {
		return rows, nil
	}
	return rows, slug(rows[len(rows)-1])
}

func (s *store) showArtifact(w http.ResponseWriter, r *http.Request) {
	workspace, ok := authenticate(w, r)
	if !ok {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	a := s.find(workspace, r.PathValue("slug"))
	if a == nil {
		writeError(w, http.StatusNotFound, "not_found", "No such record.", nil)
		return
	}
	if s.refuseIfGone(w, a) {
		return
	}
	writeJSON(w, http.StatusOK, serializeArtifact(a))
}

// destroyArtifact is the takedown. The bytes leave storage at once and what
// stays behind is a tombstone, so a read afterwards is a 410 rather than a 404 —
// the link is already pasted somewhere, and its reader deserves the difference.
//
// Immediate and unrecoverable by design. This is the path someone reaches for
// when a secret was uploaded by accident, so it must not route through any
// window that could give the bytes back.
//
// 204 rather than the artifact: there is nothing left to serialize, and a url
// and markdown naming bytes that are gone would be a lie.
func (s *store) destroyArtifact(w http.ResponseWriter, r *http.Request) {
	workspace, ok := authenticate(w, r)
	if !ok {
		return
	}

	var body struct {
		ClaimToken string `json:"claim_token"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body)

	// A keyless caller's authority is the claim token, and the slug will not do
	// even though it does for reading: a slug travels in whatever the link was
	// pasted into, so a reader of the paste must not be able to destroy what they
	// read.
	if workspace == "" && body.ClaimToken == "" {
		writeError(w, http.StatusBadRequest, "parameter_missing",
			"Missing required parameter: claim_token.", nil)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	a := s.artifacts[r.PathValue("slug")]
	if !authorizedToTakeDown(a, workspace, body.ClaimToken) {
		// A slug that does not exist, one belonging to another workspace, and a
		// token that does not match are all the same answer: a wrong guess learns
		// nothing from the difference.
		writeError(w, http.StatusNotFound, "not_found", "No such record.", nil)
		return
	}

	// Idempotent, like finalizing: taking down what is already down is a success,
	// and the deleted_at already recorded is the one that is true.
	if a.deletedAt.IsZero() {
		delete(s.objects, a.storageKey)
		a.deletedAt = s.now().UTC()
		// A takedown spends whatever upload URL is outstanding, so bytes cannot
		// land under a tombstone and resurrect the link. Real storage governs that
		// with the signature's own window instead, which a stand-in has no way to
		// reach — refusing outright is the stricter of the two, and no client that
		// takes an artifact down goes on to upload to it.
		a.uploadTok = ""
	}
	w.WriteHeader(http.StatusNoContent)
}

// authorizedToTakeDown reports whether the request carries an authority over
// this artifact. A key's is the workspace it acts in; a claim token's is the one
// artifact it was issued for.
//
// A spent token is no authority at all: claiming clears the digest in the real
// registry, and the artifact has left the anonymous workspace by then anyway, so
// a claimed artifact answers only to the key that now holds it.
//
// A tombstone still answers to both, which is what makes a retried takedown a
// success rather than a 404.
func authorizedToTakeDown(a *artifact, workspace, claimToken string) bool {
	switch {
	case a == nil:
		return false
	case workspace != "":
		return a.workspace == workspace
	default:
		return a.claimHash != "" && !a.claimed &&
			a.claimHash == sha256Hex([]byte(claimToken))
	}
}

// refuseIfGone answers for an artifact the API will not act on any further, and
// reports whether it did. Reading, finalizing, claiming and naming a run all go
// through here, so gone means the same thing on every one of them.
//
// Takedown is reported first because an artifact can be both, and the one
// somebody decided is the truer answer.
func (s *store) refuseIfGone(w http.ResponseWriter, a *artifact) bool {
	switch {
	case !a.deletedAt.IsZero():
		writeError(w, http.StatusGone, "taken_down",
			fmt.Sprintf("%s was taken down at %s", a.Slug,
				a.deletedAt.Format(time.RFC3339Nano)), nil)
	case s.expired(a):
		writeError(w, http.StatusGone, "expired",
			fmt.Sprintf("%s expired at %v", a.Slug, a.ExpiresAt), nil)
	default:
		return false
	}
	return true
}

// claimArtifact moves an anonymous artifact into the key's workspace, where it
// stops expiring. Needs a key: the key is what says which workspace.
func (s *store) claimArtifact(w http.ResponseWriter, r *http.Request) {
	workspace, ok := requireKey(w, r)
	if !ok {
		return
	}

	var body struct {
		ClaimToken string `json:"claim_token"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body)
	if body.ClaimToken == "" {
		writeError(w, http.StatusBadRequest, "parameter_missing",
			"Missing required parameter: claim_token.", nil)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	a := s.artifacts[r.PathValue("slug")]
	// The token is checked before anything else: even an artifact the workspace
	// already holds does not answer 200 to a token that was never its own.
	//
	// Stricter than the registry, deliberately and not by oversight. There the
	// retry is a bare `find_by(slug:)` in the caller's workspace — no token read
	// at all — so a wrong token, a missing one, and an artifact that was never
	// anonymous all answer 200. Requiring the token here means a garbage one
	// cannot ride the retry-after-success affordance, which is the property worth
	// testing; the cost is that these four cases answer 404 or 400 against --dev
	// where production answers 200. Nothing the CLI does reaches them.
	match := a != nil && a.claimHash != "" && a.claimHash == sha256Hex([]byte(body.ClaimToken))
	// A retry after a successful claim is the same success rather than a 404 —
	// the hash is kept, not cleared, so the retry can still be told apart from
	// a wrong token.
	//
	// This has to stay ahead of the 404 gate below, tombstone check included. The
	// registry answers a retry from `Current.workspace.artifacts.find_by`, which
	// is not scoped to `live`, so an artifact this workspace claimed and later
	// took down still answers 200 here — the `live` scope only governs the branch
	// that spends a token, which a retry no longer reaches.
	if match && a.claimed && a.workspace == workspace {
		writeJSON(w, http.StatusOK, serializeArtifact(a))
		return
	}
	// A slug that does not exist, a token that does not match it, an artifact
	// that was never anonymous, one already claimed by someone else, and a
	// tombstone are all the same answer.
	//
	// A tombstone belongs in that list rather than in the gone check below,
	// because the registry reaches this through Artifact.claimable, which narrows
	// to `live` — so a taken-down artifact is not found rather than gone. Nothing
	// claims an artifact back out of a takedown, and answering `taken_down` here
	// would have the client branch differently against --dev than production.
	if !match || a.claimed || !a.deletedAt.IsZero() {
		writeError(w, http.StatusNotFound, "not_found", "No such record.", nil)
		return
	}
	// Only an expiry reaches this now, which is all claim! can meet for the same
	// reason: the takedown case never gets past the live scope above.
	if s.refuseIfGone(w, a) {
		return
	}

	a.workspace = workspace
	a.ExpiresAt = nil
	a.claimed = true // a token is good once
	writeJSON(w, http.StatusOK, serializeArtifact(a))
}

// attachRun puts an artifact under a run after the fact, which is how an upload
// that was anonymous at create time ever gets one: it could not name a run then,
// and claiming it does not give it one.
//
// A keyless request is a plain 401, not the run_needs_key a keyless push naming a
// run answers with: the registry requires a key on this route the ordinary way,
// and raises run_needs_key only where an upload may legitimately arrive without
// one. Answering something friendlier here would make --dev disagree with
// production about the code a client branches on.
//
// A finished run still accepts one, deliberately: the case this exists for is a
// CI job whose run closed long before anyone got round to claiming the anonymous
// upload it left behind, and refusing then would leave that upload with nowhere
// to go for good.
func (s *store) attachRun(w http.ResponseWriter, r *http.Request) {
	workspace, ok := requireKey(w, r)
	if !ok {
		return
	}

	var body struct {
		Run string `json:"run"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body)
	if body.Run == "" {
		writeError(w, http.StatusBadRequest, "parameter_missing",
			"Missing required parameter: run.", nil)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Both slugs are looked up in the request's own workspace, so another
	// workspace's artifact and another workspace's run both read as not existing —
	// and an unclaimed anonymous artifact is not attachable at all.
	a := s.find(workspace, r.PathValue("slug"))
	if a == nil {
		writeError(w, http.StatusNotFound, "not_found", "No such record.", nil)
		return
	}
	// Gone is gone on every endpoint rather than only the ones that can meet a
	// gone artifact today. A takedown is reachable here — this route needs a key,
	// and so does taking down a workspace's own artifact — while an expiry is not,
	// since only a keyless upload gets one. Both go through the same check anyway,
	// so the meaning stays uniform if an ephemeral artifact ever does reach a keyed
	// workspace.
	if s.refuseIfGone(w, a) {
		return
	}
	if existing, ok := s.runs[body.Run]; !ok || existing.workspace != workspace {
		writeError(w, http.StatusNotFound, "not_found", "No such record.", nil)
		return
	}

	// Set, not appended to: an artifact belongs to one run, so a PUT naming a
	// different one moves it rather than failing. That is what makes the call
	// idempotent, and it is the only reading under which a retry whose first
	// response was lost is a success.
	a.Run = body.Run
	writeJSON(w, http.StatusOK, serializeArtifact(a))
}

func (s *store) createRun(w http.ResponseWriter, r *http.Request) {
	workspace, ok := requireKey(w, r)
	if !ok {
		return
	}

	var body struct {
		Run struct {
			Metadata json.RawMessage `json:"metadata"`
		} `json:"run"`
	}
	// A body that does not parse is refused, exactly as createArtifact refuses
	// one — an empty body is fine, a run needs no metadata.
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "parameter_missing",
			"Missing required parameter: run.", nil)
		return
	}

	metadata := body.Run.Metadata
	if !json.Valid(metadata) {
		metadata = json.RawMessage("{}")
	}

	attempt, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	requestHash := sha256Hex(metadata)

	now := s.now().UTC().Format(time.RFC3339Nano)
	entry := &run{
		Slug:      generateSlug("run"),
		Status:    "open",
		StartedAt: now,
		CreatedAt: now,
		Metadata:  metadata,
		workspace: workspace,
	}

	s.mu.Lock()
	// A run has no upload to close and no tombstone to trip over, so a replay is
	// simply the run the first attempt opened — however far through its lifecycle
	// it has since gone.
	if attempt != "" {
		if found, matches := s.replay("run", workspace, attempt, requestHash); found != nil {
			if opened, live := s.runs[found.run]; live {
				s.mu.Unlock()
				if !matches {
					keyReused(w, opened.Slug)
					return
				}
				writeJSON(w, http.StatusCreated, opened)
				return
			}
		}
	}
	s.runsOpen++
	entry.seq = s.runsOpen
	s.runs[entry.Slug] = entry
	if attempt != "" {
		s.remember("run", workspace, attempt, requestHash, &answered{run: entry.Slug})
	}
	s.mu.Unlock()

	writeJSON(w, http.StatusCreated, entry)
}

// listRuns is one page of a workspace's runs, newest first.
func (s *store) listRuns(w http.ResponseWriter, r *http.Request) {
	workspace, ok := requireKey(w, r)
	if !ok {
		return
	}
	limit := pageLimit(r)

	s.mu.Lock()
	defer s.mu.Unlock()

	var owned []*run
	for _, entry := range s.runs {
		if entry.workspace == workspace {
			owned = append(owned, entry)
		}
	}
	slices.SortFunc(owned, func(x, y *run) int { return y.seq - x.seq })

	if before := r.URL.Query().Get("before"); before != "" {
		// Looked up in the same workspace as the page, so a cursor from another
		// tenant reads as simply not existing.
		cursor := s.findRun(workspace, before)
		if cursor == nil {
			writeError(w, http.StatusNotFound, "not_found", "No such record.", nil)
			return
		}
		owned = slices.DeleteFunc(owned, func(entry *run) bool { return entry.seq >= cursor.seq })
	}

	owned, next := paginate(owned, limit, func(entry *run) string { return entry.Slug })
	// Serialized as itself rather than through a map, since a run's JSON shape is
	// the struct's own tags.
	page := make([]*run, 0, len(owned))
	page = append(page, owned...)
	writeJSON(w, http.StatusOK, map[string]any{"runs": page, "next": next})
}

func (s *store) showRun(w http.ResponseWriter, r *http.Request) {
	workspace, ok := requireKey(w, r)
	if !ok {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	entry := s.findRun(workspace, r.PathValue("slug"))
	if entry == nil {
		writeError(w, http.StatusNotFound, "not_found", "No such record.", nil)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// listRunArtifacts is what one run produced — a collection of the run rather
// than a filter on the workspace listing, so the run is looked up first and an
// unknown slug is a 404. A filter would answer an empty page instead, which a
// client cannot tell apart from a run that genuinely produced nothing.
func (s *store) listRunArtifacts(w http.ResponseWriter, r *http.Request) {
	workspace, ok := requireKey(w, r)
	if !ok {
		return
	}
	limit := pageLimit(r)
	runSlug := r.PathValue("slug")

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.findRun(workspace, runSlug) == nil {
		writeError(w, http.StatusNotFound, "not_found", "No such record.", nil)
		return
	}

	// Belonging to the run is what selects these, and the workspace is checked
	// anyway. It cannot currently differ — an artifact only joins a run through a
	// keyed request in that same workspace, and a keyless upload is refused a run
	// outright — so the second half never fires today. It is here because this is
	// the boundary between tenants, and a boundary that holds by invariant fails
	// silently when the invariant moves.
	var made []*artifact
	for _, a := range s.artifacts {
		if a.Run == runSlug && a.workspace == workspace && a.deletedAt.IsZero() {
			made = append(made, a)
		}
	}
	slices.SortFunc(made, func(x, y *artifact) int { return y.seq - x.seq })

	if before := r.URL.Query().Get("before"); before != "" {
		// Looked up among the run's own, so a cursor from outside this run reads as
		// simply not existing.
		cursor := s.artifacts[before]
		if cursor == nil || cursor.Run != runSlug {
			writeError(w, http.StatusNotFound, "not_found", "No such record.", nil)
			return
		}
		made = slices.DeleteFunc(made, func(a *artifact) bool { return a.seq >= cursor.seq })
	}

	made, next := paginate(made, limit, func(a *artifact) string { return a.Slug })
	page := make([]map[string]any, 0, len(made))
	for _, a := range made {
		page = append(page, serializeArtifact(a))
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": page, "next": next})
}

// findRun scopes a lookup to the workspace the request belongs to, so a slug
// from another one reads as simply not existing rather than as forbidden —
// which would confirm that it does.
func (s *store) findRun(workspace, slug string) *run {
	entry := s.runs[slug]
	if entry == nil || entry.workspace != workspace {
		return nil
	}
	return entry
}

func (s *store) finishRun(w http.ResponseWriter, r *http.Request) {
	workspace, ok := requireKey(w, r)
	if !ok {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	entry := s.findRun(workspace, r.PathValue("slug"))
	if entry == nil {
		writeError(w, http.StatusNotFound, "not_found", "No such record.", nil)
		return
	}
	// Idempotent: finishing a finished run keeps the moment it first finished.
	if entry.Status != "finished" {
		entry.Status = "finished"
		entry.FinishedAt = s.now().UTC().Format(time.RFC3339Nano)
	}
	writeJSON(w, http.StatusOK, entry)
}

// find scopes a lookup to the workspace the request belongs to, so a slug from
// another one reads as simply not existing.
func (s *store) find(workspace, slug string) *artifact {
	if workspace == "" {
		workspace = anonymousWorkspace
	}
	a := s.artifacts[slug]
	if a == nil || a.workspace != workspace {
		return nil
	}
	return a
}

// authenticate resolves the workspace a request acts in. An empty workspace with
// ok is a keyless request; a malformed Authorization header is a 401 rather than
// a silent fall back to anonymous, since falling back would hand the client an
// ephemeral artifact when it asked for an owned one.
func authenticate(w http.ResponseWriter, r *http.Request) (workspace string, ok bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", true
	}
	token, found := bearer(header)
	if !found {
		writeUnauthorized(w)
		return "", false
	}
	return workspaceFor(token), true
}

// requireKey is authenticate for the endpoints that cannot work without one.
func requireKey(w http.ResponseWriter, r *http.Request) (workspace string, ok bool) {
	token, found := bearer(r.Header.Get("Authorization"))
	if !found {
		writeUnauthorized(w)
		return "", false
	}
	return workspaceFor(token), true
}

func bearer(header string) (string, bool) {
	const prefix = "bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	return token, token != ""
}

// workspaceFor derives a stable workspace from the token, so two different keys
// cannot see each other's artifacts — which is the property worth testing.
func workspaceFor(token string) string {
	return "ws_" + sha256Hex([]byte(token))[:20]
}

// showKey lets the CLI self-check its key before doing any work. No key is a
// 401 — there is no key to report — and any bearer token resolves to the same
// derived workspace the artifact endpoints use, so `auth verify` and a push
// agree about where an upload would land.
//
// Nothing says "valid": a key the registry would refuse never gets this far, so
// a 200 is the answer.
func showKey(w http.ResponseWriter, r *http.Request) {
	token, found := bearer(r.Header.Get("Authorization"))
	if !found {
		writeUnauthorized(w)
		return
	}
	sum := sha256Hex([]byte(token))
	writeJSON(w, http.StatusOK, map[string]any{
		// Derived, never the token itself — a key ID ends up in logs and output.
		"key_id":    "key_" + sum[:8],
		"name":      "local",
		"workspace": workspaceFor(token),
	})
}

func writeUnauthorized(w http.ResponseWriter) {
	writeError(w, http.StatusUnauthorized, "unauthorized",
		"Provide a valid API key as `Authorization: Bearer krowk_sk_...`.", nil)
}

func (s *store) expired(a *artifact) bool {
	iso, ok := a.ExpiresAt.(string)
	if !ok {
		return false
	}
	at, err := time.Parse(time.RFC3339Nano, iso)
	return err == nil && at.Before(s.now())
}

func serializeArtifact(a *artifact) map[string]any {
	return map[string]any{
		"slug":         a.Slug,
		"state":        a.State,
		"filename":     a.Filename,
		"content_type": a.ContentType,
		"byte_size":    a.ByteSize,
		"checksum":     a.Checksum,
		"region":       a.Region,
		"run":          a.Run,
		"url":          a.URL,
		"markdown":     a.Markdown,
		"expires_at":   a.ExpiresAt,
		"created_at":   a.CreatedAt,
	}
}

// markdown is ready to paste into a pull request: an image embeds, anything else
// becomes a plain link.
func markdown(filename, contentType, url string) string {
	if strings.HasPrefix(contentType, "image/") {
		return fmt.Sprintf("![%s](%s)", filename, url)
	}
	return fmt.Sprintf("[%s](%s)", filename, url)
}

// safeFilename mirrors the real registry's: keys are attacker-influenced, so a
// name like "../../other" must not let one artifact write over another's key.
func safeFilename(filename string) string {
	base := path.Base(strings.ReplaceAll(filename, `\`, "/"))
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			return r
		}
		return '-'
	}, base)
	cleaned = strings.Trim(cleaned, "-.")
	if cleaned == "" {
		return "file"
	}
	return cleaned
}

const slugAlphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// generateSlug matches the real registry's shape: a prefix and 21 base58
// characters, so a slug is recognisable and never guessable.
func generateSlug(prefix string) string {
	b := make([]byte, 21)
	if _, err := rand.Read(b); err != nil {
		panic(err) // a stand-in with no randomness cannot issue slugs
	}
	for i, v := range b {
		b[i] = slugAlphabet[int(v)%len(slugAlphabet)]
	}
	return prefix + "_" + string(b)
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// clientAddress is where a request came from, which is the only identity a
// keyless caller has to scope an Idempotency-Key by.
func clientAddress(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// idempotencyKey reads the header a client names its attempt with, and reports
// whether the request may go on.
//
// A header sent empty is refused rather than treated as absent, exactly as the
// real registry refuses it: the client asked for the guarantee and would not be
// getting it, and a retry that quietly stops deduplicating surfaces as a
// surprise on a bill rather than as an error.
func idempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	values, sent := r.Header["Idempotency-Key"]
	if !sent {
		return "", true
	}
	key := ""
	if len(values) > 0 {
		key = strings.TrimSpace(values[0])
	}
	if key == "" {
		writeError(w, http.StatusBadRequest, "parameter_missing",
			"Missing required parameter: Idempotency-Key.", nil)
		return "", false
	}
	return key, true
}

// replay finds what a key already created, and reports whether the payload it
// arrived with is the one it was first used for.
//
// No expiry, deliberately. The real registry promises a key is honoured for *at
// least* a day and sweeps it hourly after that, so a key it still answers for is
// one this process must answer for too — cutting them off at exactly a day here
// would have the stand-in refuse replays the registry allows, which is the drift
// that matters. Honouring them for as long as the process lives errs the safe way,
// and nothing a client does depends on a key going stale.
func (s *store) replay(kind, scope, key, requestHash string) (*answered, bool) {
	found, ok := s.idempotent[kind+"\n"+scope+"\n"+key]
	if !ok {
		return nil, false
	}
	return found, found.requestHash == requestHash
}

func (s *store) remember(kind, scope, key, requestHash string, entry *answered) {
	entry.requestHash = requestHash
	s.idempotent[kind+"\n"+scope+"\n"+key] = entry
}

// keyReused is the one refusal an Idempotency-Key adds to the wire. 409 rather
// than 422 for the same reason already_finalized is one: nothing about the
// payload is wrong, and what refuses it is a record that already exists.
func keyReused(w http.ResponseWriter, slug string) {
	writeError(w, http.StatusConflict, "idempotency_key_reused",
		"this Idempotency-Key already created "+slug+
			" from a different request — send a new key to create something else", nil)
}

// Checksums travel as lowercase hex in the API — readable, and what sha256sum
// prints — but S3 wants them base64 encoded, so the upload header carries that.
func base64Sum(hexSum string) string {
	raw, err := hex.DecodeString(hexSum)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func itoa(n int64) string { return fmt.Sprintf("%d", n) }

func site(r *http.Request, override string) string {
	if override != "" {
		return strings.TrimRight(override, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// writeError is the one error envelope the whole API answers in, so a client can
// branch on error.code instead of parsing prose.
func writeError(w http.ResponseWriter, status int, code, message string, details map[string]any) {
	payload := map[string]any{"code": code, "message": message}
	if len(details) > 0 {
		payload["details"] = details
	}
	writeJSON(w, status, map[string]any{"error": payload})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}

// writeXMLError is object storage's error shape, not the registry's.
func writeXMLError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<Error><Code>%s</Code></Error>", code)
}
