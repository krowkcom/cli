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
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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
}

type store struct {
	mu        sync.Mutex
	artifacts map[string]*artifact
	runs      map[string]*run
	objects   map[string][]byte
	created   int
	now       func() time.Time
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
		artifacts: map[string]*artifact{},
		runs:      map[string]*run{},
		objects:   map[string][]byte{},
		now:       now,
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
	mux.HandleFunc("PUT /v1/runs/{slug}/completion", s.finishRun)
	mux.HandleFunc("PATCH /v1/runs/{slug}/completion", s.finishRun)

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

	s.mu.Lock()
	defer s.mu.Unlock()

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

	payload := map[string]any{}
	var claimToken string
	if anonymous {
		a.ExpiresAt = now.Add(ephemeralLifetime).Format(time.RFC3339Nano)
		claimToken = "krowk_claim_" + randomToken()
		a.claimHash = sha256Hex([]byte(claimToken))
	}
	s.artifacts[slug] = a

	for k, v := range serializeArtifact(a) {
		payload[k] = v
	}
	payload["upload"] = map[string]any{
		"method":     "PUT",
		"url":        url + "?upload_token=" + a.uploadTok,
		"headers":    map[string]string{"Content-Type": a.ContentType, "Content-Length": itoa(a.ByteSize)},
		"expires_at": a.uploadTil.Format(time.RFC3339Nano),
	}
	payload["next_step"] = "PUT the file to upload.url with the headers in upload.headers, " +
		"then PUT /v1/artifacts/" + slug + "/finalization"
	if claimToken != "" {
		payload["claim_token"] = claimToken
	}

	writeJSON(w, http.StatusCreated, payload)
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
	if a.State == "ready" {
		writeJSON(w, http.StatusOK, serializeArtifact(a))
		return
	}
	if s.expired(a) {
		writeError(w, http.StatusGone, "expired",
			fmt.Sprintf("%s expired at %v", a.Slug, a.ExpiresAt), nil)
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

	limit := defaultPageSize
	if requested, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil {
		// Clamped rather than refused: a caller asking for more than we serve gets
		// the most we serve, which is what it wanted.
		limit = min(max(requested, 1), maxPageSize)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var owned []*artifact
	for _, a := range s.artifacts {
		if a.workspace == workspace {
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

	// next is the slug to pass back as before, and is null on the last page.
	// Present whenever the page came back full, as the registry's own does — so a
	// total that is an exact multiple of the limit costs one extra empty page
	// rather than a count query on every listing.
	if len(owned) > limit {
		owned = owned[:limit]
	}
	var next any
	if len(owned) == limit {
		next = owned[len(owned)-1].Slug
	}

	page := make([]map[string]any, 0, len(owned))
	for _, a := range owned {
		page = append(page, serializeArtifact(a))
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": page, "next": next})
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
	if s.expired(a) {
		writeError(w, http.StatusGone, "expired",
			fmt.Sprintf("%s expired at %v", a.Slug, a.ExpiresAt), nil)
		return
	}
	writeJSON(w, http.StatusOK, serializeArtifact(a))
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
	match := a != nil && a.claimHash != "" && a.claimHash == sha256Hex([]byte(body.ClaimToken))
	// A retry after a successful claim is the same success rather than a 404 —
	// the hash is kept, not cleared, so the retry can still be told apart from
	// a wrong token.
	if match && a.claimed && a.workspace == workspace {
		writeJSON(w, http.StatusOK, serializeArtifact(a))
		return
	}
	// A slug that does not exist, a token that does not match it, an artifact
	// that was never anonymous, and one already claimed by someone else are all
	// the same answer.
	if !match || a.claimed {
		writeError(w, http.StatusNotFound, "not_found", "No such record.", nil)
		return
	}
	if s.expired(a) {
		writeError(w, http.StatusGone, "expired",
			fmt.Sprintf("%s expired at %v", a.Slug, a.ExpiresAt), nil)
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
// Keyless is refused with the same run_needs_key createArtifact answers with
// rather than a 401: what is wrong is not the missing key by itself but that a
// run belongs to a workspace and a keyless request has none.
//
// A finished run still accepts one, deliberately: the case this exists for is a
// CI job whose run closed long before anyone got round to claiming the anonymous
// upload it left behind, and refusing then would leave that upload with nowhere
// to go for good.
func (s *store) attachRun(w http.ResponseWriter, r *http.Request) {
	workspace, ok := authenticate(w, r)
	if !ok {
		return
	}
	if workspace == "" {
		writeError(w, http.StatusUnprocessableEntity, "run_needs_key",
			"Attaching an artifact to a run needs an API key — a keyless upload has no workspace.", nil)
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
	if existing, ok := s.runs[body.Run]; !ok || existing.workspace != workspace {
		writeError(w, http.StatusNotFound, "not_found", "No such record.", nil)
		return
	}

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
	s.runs[entry.Slug] = entry
	s.mu.Unlock()

	writeJSON(w, http.StatusCreated, entry)
}

func (s *store) finishRun(w http.ResponseWriter, r *http.Request) {
	workspace, ok := requireKey(w, r)
	if !ok {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	entry := s.runs[r.PathValue("slug")]
	if entry == nil || entry.workspace != workspace {
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
