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
	"html"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
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

	// UploadURLLifetime is how long a presigned URL this stand-in hands out stays
	// good, matching the real 15 minutes. Exported alongside HandlerWithClock
	// because the two together are how a test invalidates a signature: no other
	// seam makes 15 minutes elapse inside one, and the recovery from a lapsed
	// signature is a path worth exercising from outside this package.
	UploadURLLifetime = 15 * time.Minute

	// How long a keyless upload survives, matching Artifact::EPHEMERAL_LIFETIME.
	ephemeralLifetime = 24 * time.Hour

	// How long a browser login stays open, matching the real quarter hour: long
	// enough to find the tab, short enough that an abandoned one is not still
	// approvable after lunch.
	cliAuthorizationLifetime = 15 * time.Minute

	// How long a lapsed login is kept before it is swept, so that a client polling
	// just after the window still learns the window closed rather than that no such
	// login exists.
	cliAuthorizationGrace = time.Hour

	// How often this stand-in asks to be polled, in seconds. One rather than the
	// real five, because everything here is on loopback and a developer watching
	// the flow by hand should not spend most of it waiting.
	cliAuthorizationInterval = 1

	// How many artifacts a page holds, mirroring the registry's own bounds. The
	// caller picks, so the ceiling is enforced rather than trusted.
	defaultPageSize = 50
	maxPageSize     = 100

	// The workspace slug keyless uploads land in. One shared workspace, as in
	// the real registry, so the storage keys look the same — padded to canon's
	// 24 characters so it reads as a slug rather than as an exception to one.
	anonymousWorkspace = "ws_anonymous000000000000000"
)

type artifact struct {
	Slug        string `json:"slug"`
	State       string `json:"state"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	ByteSize    int64  `json:"byte_size"`
	Checksum    string `json:"checksum,omitempty"`
	Region      string `json:"region"`
	// Run is the slug of the run this artifact belongs to, empty for none. On
	// the wire it goes out as the nested run object serializeArtifact builds,
	// which is why this field is not serialized from here.
	Run string `json:"-"`
	// URL is the card page and FileURL is the object in storage. Two fields
	// because they are two different things: the card is what gets pasted and
	// unfurled, the file is where the bytes are and what an image embed has to
	// name. Only one host serves both here; in production they are krowk.com
	// and the CDN.
	URL       string `json:"url"`
	FileURL   string `json:"file_url"`
	Markdown  string `json:"markdown"`
	ExpiresAt any    `json:"expires_at"`
	CreatedAt string `json:"created_at"`

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

// authorization is one browser login in flight.
//
// Two capabilities, held apart on purpose. slug is what the CLI polls and is
// never shown in a browser; code is what a person confirms on the page and can
// only approve or deny. So the half that travels through a terminal cannot be
// turned into a key by whoever reads it — which is only true if the code is
// never accepted as authority to *collect*, and it is not: the poll is by slug
// and nothing else.
type authorization struct {
	slug      string
	code      string
	state     string
	createdAt time.Time

	// token is the plaintext key, and the read that hands it over removes it.
	// One-shot delivery is not a rule applied to the record — there is simply
	// nothing left to hand over a second time.
	token     string
	keyID     string
	workspace string
	// spent records that the key has already been collected, because an empty
	// token would otherwise be indistinguishable from one that was never minted.
	spent bool
}

type store struct {
	mu        sync.Mutex
	artifacts map[string]*artifact
	runs      map[string]*run
	objects   map[string][]byte
	// Browser logins, by slug. Looked up by code as well — the page a person
	// approves on knows only that half — but a map keyed by slug and scanned for a
	// code is the right shape for a handful of records that live a quarter hour.
	authorizations map[string]*authorization
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
		artifacts:      map[string]*artifact{},
		runs:           map[string]*run{},
		objects:        map[string][]byte{},
		authorizations: map[string]*authorization{},
		idempotent:     map[string]*answered{},
		now:            now,
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

	// The upload of an artifact, handed out again. A presigned URL lasts 15
	// minutes while the artifact waits for its bytes far longer, so a lapsed
	// signature has to be recoverable without declaring a second artifact — which
	// would be a second slug, and a dead link in whatever the first was pasted
	// into.
	//
	// A POST though nothing is recorded: what comes back is a new capability, and
	// asking for one means presenting a credential — which for a keyless caller is
	// the claim token, and a secret belongs in a body.
	mux.HandleFunc("POST /v1/artifacts/{slug}/upload", s.presignUpload)

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

	// A browser login. Creating one takes no key — the whole point is a machine
	// that has none — and reading one back is authorised by its slug, which is a
	// capability the caller was handed and nobody else ever sees.
	mux.HandleFunc("POST /v1/cli/authorizations", func(w http.ResponseWriter, r *http.Request) {
		s.createCLIAuthorization(w, site(r, siteURL))
	})
	mux.HandleFunc("GET /v1/cli/authorizations/{slug}", s.showCLIAuthorization)

	// The approval screen, which in production is a page on the app surface with a
	// signed-in person in front of it. This is the stand-in's substitute: a code to
	// compare and two buttons, on a path that says it is not the API and not the
	// website either, so `make mock` gives a working flow by hand and a test has
	// something to press. Mirroring the real screen any further would make this a
	// second implementation of it, which is how a stand-in starts lying.
	mux.HandleFunc("GET /_approve/cli/authorizations/new", s.cliAuthorizationPage)
	mux.HandleFunc("POST /_approve/cli/authorizations/{code}/approval",
		func(w http.ResponseWriter, r *http.Request) { s.decideCLIAuthorization(w, r, true) })
	mux.HandleFunc("POST /_approve/cli/authorizations/{code}/denial",
		func(w http.ResponseWriter, r *http.Request) { s.decideCLIAuthorization(w, r, false) })

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

	// The card page: what `url` now points at, and the whole reason it stopped
	// pointing at the object. Server-rendered, sessionless and keyless, because
	// an unfurler is an HTTP client with no credentials that reads the tags out
	// of the first response and never runs any script. In production the Nuxt
	// site serves this off the registry's data; here it is the same origin, so
	// a paste out of `krowk registry serve` unfurls the way production does.
	mux.HandleFunc("GET /a/{slug}", s.artifactPage)

	// A path that matches nothing, which includes a known path asked for with a
	// verb it does not serve — Go's mux falls through to this pattern rather
	// than answering 405, and so does the Rails router the real registry runs.
	//
	// `no_such_endpoint` rather than `not_found`: an unknown slug and an unknown
	// path are different mistakes, and a client that cannot tell them apart
	// sends someone hunting for a typo in a slug when the base URL is wrong.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no_such_endpoint",
			"No such endpoint. Check the path and the method — GET / lists the versions this API serves.", nil)
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
		if unreadableBody(w, err) {
			return
		}
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
	// The card page is the link, the storage path is where the bytes are. The
	// real registry serves the first from krowk.com and the second from the
	// CDN; here one origin serves both, which is enough for a paste against
	// `krowk registry serve` to behave the way a paste against production does.
	fileURL := site + "/_storage/" + key
	url := site + "/a/" + slug
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
		FileURL:     fileURL,
		Markdown:    markdown(in.Filename, in.ContentType, fileURL, url),
		CreatedAt:   now.Format(time.RFC3339Nano),
		workspace:   workspace,
		uploadTok:   randomToken(),
		uploadTil:   now.Add(UploadURLLifetime),
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
	for k, v := range s.serializeArtifact(a) {
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
		"url":        a.FileURL + "?upload_token=" + a.uploadTok,
		"headers":    uploadHeaders,
		"expires_at": a.uploadTil.Format(time.RFC3339Nano),
	}
	// The recovery is spelled out, because the caller is often an agent following
	// instructions rather than a client written against documentation — and the
	// instruction it needs when a signature lapses is the one call that keeps the
	// slug. Whether it has to carry a claim token follows from the artifact being
	// ephemeral, not from a token being in hand: this same payload is served on
	// every represign, where the plaintext token is long gone.
	withToken := ""
	if a.ExpiresAt != nil {
		withToken = " with claim_token"
	}
	payload["next_step"] = "PUT the file to upload.url with the headers in upload.headers, " +
		"then PUT /v1/artifacts/" + a.Slug + "/finalization. " +
		"If upload.url expires first, POST /v1/artifacts/" + a.Slug + "/upload" + withToken +
		" for a fresh one — the slug does not change"
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
	a.uploadTil = now.Add(UploadURLLifetime)

	// The claim token is not re-issued. Re-minting one would void the token the
	// first response may already have delivered, and the row keeps only its digest
	// regardless — as in the real registry.
	writeJSON(w, http.StatusCreated, s.declared(a, ""))
	return true
}

// presignUpload mints the upload of an artifact again: same slug, same storage
// key, same declared size and digest, so the link that may already be pasted
// somewhere is the one the bytes land behind.
//
// 200 rather than 201, and the artifact rather than an upload of its own: what
// comes back is what a create answers with, minted again. The upload is not a
// record to point at.
func (s *store) presignUpload(w http.ResponseWriter, r *http.Request) {
	workspace, ok := authenticate(w, r)
	if !ok {
		return
	}

	var body struct {
		ClaimToken string `json:"claim_token"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); unreadableBody(w, err) {
		return
	}

	// A keyless caller's authority is the claim token, and the slug will not do
	// even though it does for reading. Reading a keyless artifact goes on the slug
	// because the bytes are public on the CDN regardless — but a presigned PUT is
	// not a read, it is permission to decide what those bytes are, and the slug is
	// in whatever the link was pasted into.
	if workspace == "" && body.ClaimToken == "" {
		writeError(w, http.StatusBadRequest, "parameter_missing",
			"Missing required parameter: claim_token.", nil)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	a := s.artifacts[r.PathValue("slug")]
	if !authorizedToWrite(a, workspace, body.ClaimToken) {
		// A slug that does not exist, one belonging to another workspace, and a
		// token that does not match are all the same answer: a wrong guess learns
		// nothing from the difference.
		writeError(w, http.StatusNotFound, "not_found", "No such record.", nil)
		return
	}
	if s.refuseIfGone(w, a) {
		return
	}
	// A ready artifact is a permalink, so there is nothing here to presign: a URL
	// over its key would be permission to swap the bytes a link already resolves
	// to.
	if a.State == "ready" {
		writeError(w, http.StatusConflict, "already_finalized",
			a.Slug+" is already finalized — declare a new artifact for new bytes", nil)
		return
	}

	// Nothing here touches created_at, and the finalize deadline is measured from
	// it: asking for a URL is not evidence the upload is coming, and a deadline any
	// client can push out by asking is not a deadline.
	a.uploadTok = randomToken()
	a.uploadTil = s.now().UTC().Add(UploadURLLifetime)

	// The claim token is not re-issued. It is shown once, by the call that minted
	// it, and the row keeps only its digest — as in the real registry, where
	// spending it here must not become a second chance to read it.
	writeJSON(w, http.StatusOK, s.declared(a, ""))
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
	var contentType string
	// The 24-hour promise covers the bytes, not just the record: an expired
	// artifact's public URL stops serving, as the real registry's lifecycle
	// rule deletes the object. Gone reads as never-there, the way storage does.
	if a := s.artifacts[r.PathValue("slug")]; a != nil {
		if s.expired(a) {
			ok = false
		}
		contentType = a.ContentType
	}
	s.mu.Unlock()

	if !ok {
		writeXMLError(w, http.StatusNotFound, "NoSuchKey")
		return
	}
	// Storage serves the type the object was stored with, and the card page's
	// og:image is only an image to an unfurler if this says so — without it Go
	// sniffs, which is close but not the contract the CDN honours.
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	_, _ = w.Write(bytes)
}

// artifactPage is the card at /a/{slug} — the page `url` names, and the one the
// whole change exists for. It is deliberately plain: an unfurler fetches it
// once with no credentials, reads the OpenGraph tags out of the markup and runs
// nothing, so there is no styling and no script here to be worth having.
//
// Public and keyless, because the slug is the capability: it is unguessable and
// the bytes behind it are public on the CDN regardless, so demanding a key here
// would only stop Slack from ever seeing the page.
func (s *store) artifactPage(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")

	s.mu.Lock()
	a := s.artifacts[slug]
	var (
		gone     string
		filename string
		fileURL  string
		cardURL  string
		image    bool
		pending  bool
		size     int64
	)
	if a != nil {
		switch {
		case !a.deletedAt.IsZero():
			gone = "taken_down"
		case s.expired(a):
			gone = "expired"
		}
		filename, fileURL, cardURL = a.Filename, a.FileURL, a.URL
		image = strings.HasPrefix(a.ContentType, "image/")
		pending = a.State != "ready"
		size = a.ByteSize
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if a == nil {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, page("Not found", nil, "<p>No such artifact.</p>"))
		return
	}
	// 410 rather than 404 on a page for the same reason as on the API: the link
	// is already pasted somewhere, and "this existed and is gone" is a different
	// thing to tell a reader than "check your typo". A taken-down artifact does
	// not name its file — that is half of what a takedown is for.
	if gone != "" {
		w.WriteHeader(http.StatusGone)
		title, body := "Taken down", "<p>This artifact was taken down.</p>"
		if gone == "expired" {
			title = filename
			body = "<p>" + html.EscapeString(filename) + " has expired.</p>"
		}
		_, _ = io.WriteString(w, page(title, map[string]string{
			"og:title": title, "og:type": "website", "og:url": cardURL,
		}, body))
		return
	}

	// A pending artifact says so. Naming its bytes in og:image would put a
	// broken image in every card built from this page, and the upload may still
	// be on its way — which is worth saying rather than rendering as breakage.
	if pending {
		_, _ = io.WriteString(w, page(filename, map[string]string{
			"og:title":       filename,
			"og:type":        "website",
			"og:url":         cardURL,
			"og:description": "Upload pending — the bytes have not landed yet.",
		}, "<p>"+html.EscapeString(filename)+" — upload pending.</p>"))
		return
	}

	tags := map[string]string{
		"og:title":       filename,
		"og:type":        "website",
		"og:url":         cardURL,
		"og:description": humanBytes(size) + " · krowk",
	}
	body := `<p><a href="` + html.EscapeString(fileURL) + `">` + html.EscapeString(filename) + `</a></p>`
	if image {
		// The image tag names the bytes, never this page: an unfurler fetches
		// og:image expecting to get image bytes back, and would get this HTML.
		tags["og:image"] = fileURL
		tags["twitter:card"] = "summary_large_image"
		body = `<p><img src="` + html.EscapeString(fileURL) + `" alt="` + html.EscapeString(filename) + `"></p>` + body
	}
	_, _ = io.WriteString(w, page(filename, tags, body))
}

// page is the whole of this stand-in's HTML: a title, the meta tags in a stable
// order so a test can read them, and a body. Sorted because Go's map iteration
// is not, and a page whose markup reshuffles between requests is one no
// assertion can pin.
func page(title string, tags map[string]string, body string) string {
	var meta strings.Builder
	for _, k := range slices.Sorted(maps.Keys(tags)) {
		meta.WriteString(`<meta property="` + html.EscapeString(k) +
			`" content="` + html.EscapeString(tags[k]) + `">` + "\n")
	}
	return "<!doctype html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n<title>" +
		html.EscapeString(title) + "</title>\n" + meta.String() + "</head>\n<body>\n" + body + "\n</body>\n</html>\n"
}

// humanBytes is the size as a card would say it. The CLI has its own, and this
// one is not shared with it: importing the output package into the stand-in
// registry would point the dependency the wrong way round — the stand-in exists
// to be something the CLI is tested against, not something it is built on.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit && exp < 3 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.0f %s", float64(n)/float64(div), []string{"KB", "MB", "GB", "TB"}[exp])
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
		writeJSON(w, http.StatusOK, s.serializeArtifact(a))
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
	writeJSON(w, http.StatusOK, s.serializeArtifact(a))
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
		page = append(page, s.serializeArtifact(a))
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
	writeJSON(w, http.StatusOK, s.serializeArtifact(a))
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
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); unreadableBody(w, err) {
		return
	}

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
	if !authorizedToWrite(a, workspace, body.ClaimToken) {
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

// authorizedToWrite reports whether the request carries an authority over this
// artifact. A key's is the workspace it acts in; a claim token's is the one
// artifact it was issued for.
//
// Shared by taking an artifact down and by minting its upload URL again, because
// they are the same question: both decide what a link resolves to, and a slug
// travels in whatever that link was pasted into. The real registry gives them one
// answer too, off the same held_by_claim_token scope.
//
// A spent token is no authority at all: claiming clears the digest in the real
// registry, and the artifact has left the anonymous workspace by then anyway, so
// a claimed artifact answers only to the key that now holds it.
//
// A tombstone still answers to both, which is what makes a retried takedown a
// success rather than a 404 — and what makes a represign of one a 410 saying it
// was taken down rather than a 404 sending someone hunting for a typo.
func authorizedToWrite(a *artifact, workspace, claimToken string) bool {
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
//
// An expiry carries the filename and when it was created, because the link is
// already pasted somewhere and the card page rendering the refusal has nothing
// else left to name the thing that used to be there. A takedown carries
// nothing: somebody asked for it to be gone, and echoing the filename back
// would undo half of that.
func (s *store) refuseIfGone(w http.ResponseWriter, a *artifact) bool {
	switch {
	case !a.deletedAt.IsZero():
		writeError(w, http.StatusGone, "taken_down",
			fmt.Sprintf("%s was taken down at %s", a.Slug,
				a.deletedAt.Format(time.RFC3339Nano)), nil)
	case s.expired(a):
		writeError(w, http.StatusGone, "expired",
			fmt.Sprintf("%s expired at %v", a.Slug, a.ExpiresAt),
			map[string]any{"filename": a.Filename, "created_at": a.CreatedAt})
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
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); unreadableBody(w, err) {
		return
	}
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
		writeJSON(w, http.StatusOK, s.serializeArtifact(a))
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
	writeJSON(w, http.StatusOK, s.serializeArtifact(a))
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
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); unreadableBody(w, err) {
		return
	}
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
	writeJSON(w, http.StatusOK, s.serializeArtifact(a))
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
		if unreadableBody(w, err) {
			return
		}
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
		page = append(page, s.serializeArtifact(a))
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
// cannot see each other's artifacts — which is the property worth testing. Hex
// is a subset of lowercase base36, so 24 of it is a slug of canon's shape.
func workspaceFor(token string) string {
	return "ws_" + sha256Hex([]byte(token))[:slugRandomLength]
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

// createCLIAuthorization opens a browser login and answers with both halves: the
// slug that collects the key, and the code a person confirms.
//
// No Idempotency-Key is read, and the real endpoint does not take one either. A
// lost response means the caller never saw the code, so the authorization it
// belongs to can never be approved — it charges nothing, reserves nothing, and
// lapses on its own, which is the difference between this and every other create
// here.
func (s *store) createCLIAuthorization(w http.ResponseWriter, site string) {
	s.mu.Lock()
	s.sweepAuthorizations()
	auth := &authorization{
		slug:      generateSlug("aut"),
		code:      s.freeCode(),
		state:     authorizationPending,
		createdAt: s.now(),
	}
	s.authorizations[auth.slug] = auth
	// Read out while the record is still held. The moment it is in the map another
	// request can reach it and change its state, and building a response out of a
	// struct this handler no longer holds is the same mistake wherever it is made —
	// nobody can know the code yet, but that is a fact about this endpoint rather
	// than about the lock.
	slug, code, state := auth.slug, auth.code, auth.state
	expires := auth.createdAt.Add(cliAuthorizationLifetime)
	s.mu.Unlock()

	writeJSON(w, http.StatusCreated, map[string]any{
		"slug":  slug,
		"state": state,
		"code":  code,
		// Only the code travels in the URL. Putting the slug there would hand the
		// browser — and its history, and whatever extension is reading it — the half
		// that collects the key.
		"verification_url": site + "/_approve/cli/authorizations/new?code=" + url.QueryEscape(code),
		"interval":         cliAuthorizationInterval,
		"expires_at":       expires.UTC().Format(time.RFC3339),
	})
}

// showCLIAuthorization is the CLI's poll, and the only way the key is ever handed
// over. It hands it over once.
//
// Spent is checked before expiry, because a client asking twice deserves to be
// told which of the two happened: a key that was collected and then sat past its
// window was still collected, and "this lapsed before it was approved" would be a
// lie about a key that is on somebody's disk. Expiry is checked before the state,
// so an approval that landed a moment too late reads as lapsed rather than as a
// key — the window is the window.
//
// Delivery is one-shot, and the token is taken out under the lock so that two
// polls arriving together cannot both be handed it. It goes back only if the
// response never made it out: a key nobody received is not a key that was
// collected, and losing a login to a dropped connection is a worse answer than
// this is complicated.
func (s *store) showCLIAuthorization(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")

	s.mu.Lock()
	auth := s.authorizations[slug]
	switch {
	case auth == nil:
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, "not_found", "No such authorization.", nil)
		return
	case auth.spent:
		s.mu.Unlock()
		writeError(w, http.StatusGone, "spent",
			"This authorization's key has already been collected, and no copy was kept.", nil)
		return
	case s.authorizationExpired(auth):
		s.mu.Unlock()
		writeError(w, http.StatusGone, "expired", "This authorization has expired.", nil)
		return
	}

	body := map[string]any{
		"slug":       auth.slug,
		"state":      auth.state,
		"expires_at": auth.createdAt.Add(cliAuthorizationLifetime).UTC().Format(time.RFC3339),
	}
	delivering := auth.state == authorizationApproved
	token := auth.token
	if delivering {
		body["token"], body["key_id"], body["workspace"] = auth.token, auth.keyID, auth.workspace
		auth.token, auth.spent = "", true
	}
	s.mu.Unlock()

	if err := encodeJSON(w, http.StatusOK, body); err != nil && delivering {
		// Nothing else can have taken it in the meantime — it was removed under the
		// lock, and this is the only path that hands it out.
		s.mu.Lock()
		auth.token, auth.spent = token, false
		s.mu.Unlock()
	}
}

// cliAuthorizationPage is where a person confirms the code. A code matching no
// login still waiting is refused outright rather than shown with buttons that
// would answer 404 once pressed.
//
// What it renders is the code on file, not the one the query string carried, and
// it is read out inside the lock: everything the page needs is copied while the
// record is held, so nothing here touches a struct another request may be
// deciding on.
func (s *store) cliAuthorizationPage(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	auth := s.findAuthorizationByCode(r.URL.Query().Get("code"))
	pending := auth != nil && auth.state == authorizationPending && !s.authorizationExpired(auth)
	code := ""
	if pending {
		code = auth.code
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if !pending {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, page("No such login",
			nil, "<p>No login is waiting on that code.</p>"))
		return
	}

	safe := url.PathEscape(code)
	_, _ = io.WriteString(w, page("Approve this login", nil,
		"<p>A terminal asked to sign in. Approve it only if this code is the one it printed.</p>\n"+
			"<p><strong>"+html.EscapeString(code)+"</strong></p>\n"+
			`<form method="post" action="/_approve/cli/authorizations/`+safe+
			`/approval"><button type="submit">Approve</button></form>`+"\n"+
			`<form method="post" action="/_approve/cli/authorizations/`+safe+
			`/denial"><button type="submit">Deny</button></form>`))
}

// decideCLIAuthorization is what the two buttons post to, and what a test presses
// instead of a person. Approving is what mints the key — the code's whole
// authority, and it stops there: nothing about this response carries the key, so
// knowing a code never yields one.
//
// The key is derived from its own token exactly as `GET /v1/key` derives one, so a
// login here and an `auth verify` afterwards agree about which key this is and
// which workspace it acts in.
func (s *store) decideCLIAuthorization(w http.ResponseWriter, r *http.Request, approve bool) {
	code := r.PathValue("code")

	s.mu.Lock()
	auth := s.findAuthorizationByCode(code)
	switch {
	case auth == nil:
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, "not_found", "No such authorization.", nil)
		return
	case s.authorizationExpired(auth):
		s.mu.Unlock()
		writeError(w, http.StatusGone, "expired", "This authorization has expired.", nil)
		return
	case auth.state != authorizationPending:
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "already_decided",
			"This authorization was already answered.", nil)
		return
	}

	decision := "denied"
	if approve {
		auth.state = authorizationApproved
		auth.token = "krowk_sk_" + randomToken()[:32]
		auth.keyID = "key_" + sha256Hex([]byte(auth.token))[:8]
		auth.workspace = workspaceFor(auth.token)
		decision = "approved"
	} else {
		auth.state = authorizationDenied
	}
	s.mu.Unlock()

	// HTML because a browser is what posted it, and the person doing the posting is
	// looking at the result. The key is not in here and never will be: it goes to
	// whoever holds the slug.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, page("Login "+decision, nil,
		"<p>Login "+decision+". Back to your terminal.</p>"))
}

// The states an authorization reports. Spent and expired are not among them: both
// are gone, and gone is a 410 rather than a body.
const (
	authorizationPending  = "pending"
	authorizationApproved = "approved"
	authorizationDenied   = "denied"
)

// authorizationExpired reports whether the window has closed. Caller holds the
// lock — the clock is the store's, and every caller is already inside it.
func (s *store) authorizationExpired(a *authorization) bool {
	return s.now().After(a.createdAt.Add(cliAuthorizationLifetime))
}

// sweepAuthorizations drops what has been lapsed long enough that nobody is still
// asking about it. The real registry sweeps on a schedule; here it runs when a
// login is opened, which is the only moment this map can grow — otherwise a
// `registry serve` left up all week accumulates every login it ever answered, and
// the lookup by code walks all of them.
//
// The grace period is what keeps `410 expired` meaningful. Reaping at the window's
// edge would have a client that polled a second too late get `404 no such
// authorization`, which is a different thing to be told: one says the window
// closed, the other says nothing was ever there. Caller holds the lock.
func (s *store) sweepAuthorizations() {
	for slug, auth := range s.authorizations {
		if s.now().After(auth.createdAt.Add(cliAuthorizationLifetime + cliAuthorizationGrace)) {
			delete(s.authorizations, slug)
		}
	}
}

// findAuthorizationByCode is the lookup the approval page needs, and the only
// place a code resolves to a record. Caller holds the lock. An empty code matches
// nothing, so a page asked for without one refuses rather than picking a login at
// random.
func (s *store) findAuthorizationByCode(code string) *authorization {
	if code == "" {
		return nil
	}
	for _, auth := range s.authorizations {
		if auth.code == code {
			return auth
		}
	}
	return nil
}

// freeCode mints a code no live authorization is already using, since two logins
// sharing one would have a person approving whichever was found first. Caller
// holds the lock.
func (s *store) freeCode() string {
	for {
		if code := generateCode(); s.findAuthorizationByCode(code) == nil {
			return code
		}
	}
}

// codeAlphabet leaves out 0/O and 1/I. A code is read off a screen and typed
// somewhere else, or read out loud, and those are the two pairs that get
// confused. 32 characters also divides 256, so picking one per random byte is
// unbiased.
const codeAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

// generateCode is two groups of four, hyphenated: short enough to compare at a
// glance, and 32^8 — over a trillion — is far more than a quarter-hour window
// leaves room to guess at.
func generateCode() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err) // a stand-in with no randomness cannot issue codes
	}
	for i, v := range b {
		b[i] = codeAlphabet[int(v)%len(codeAlphabet)]
	}
	return string(b[:4]) + "-" + string(b[4:])
}

func (s *store) expired(a *artifact) bool {
	iso, ok := a.ExpiresAt.(string)
	if !ok {
		return false
	}
	at, err := time.Parse(time.RFC3339Nano, iso)
	return err == nil && at.Before(s.now())
}

// serializeArtifact is the artifact as the API reports it. A method rather than
// a free function because `run` is now the run itself rather than its slug, and
// the run lives in the store — every caller already holds the lock.
func (s *store) serializeArtifact(a *artifact) map[string]any {
	return map[string]any{
		"slug":         a.Slug,
		"state":        a.State,
		"filename":     a.Filename,
		"content_type": a.ContentType,
		"byte_size":    a.ByteSize,
		"checksum":     a.Checksum,
		"region":       a.Region,
		"run":          s.serializeArtifactRun(a),
		"url":          a.URL,
		"file_url":     a.FileURL,
		"markdown":     a.Markdown,
		"expires_at":   a.ExpiresAt,
		"created_at":   a.CreatedAt,
	}
}

// serializeArtifactRun is the run nested inside an artifact: null when it
// belongs to none, and otherwise the slug, the metadata and when it was opened.
// Nested rather than a bare slug so a client reading an artifact back knows
// what produced it without a second call.
//
// The run being missing from the store is not possible today — an artifact only
// ever names one this store minted — but it answers null rather than a
// half-object if it ever becomes so.
func (s *store) serializeArtifactRun(a *artifact) any {
	if a.Run == "" {
		return nil
	}
	r, ok := s.runs[a.Run]
	if !ok {
		return nil
	}
	metadata := r.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage("{}")
	}
	return map[string]any{
		"slug":       r.Slug,
		"metadata":   metadata,
		"created_at": r.CreatedAt,
	}
}

// markdown is ready to paste into a pull request. An image embeds and the embed
// links through to the card page — the embed has to name the bytes, because a
// paste destination renders an image only where the link resolves to one, but
// clicking it should land on the page with the run metadata rather than on a
// bare file. Anything that cannot be embedded is a plain link to the card.
func markdown(filename, contentType, fileURL, cardURL string) string {
	label := labelEscaper.Replace(filename)
	if strings.HasPrefix(contentType, "image/") {
		return fmt.Sprintf("[![%s](%s)](%s)", label, fileURL, cardURL)
	}
	return fmt.Sprintf("[%s](%s)", label, cardURL)
}

// labelEscaper escapes what would end or nest a CommonMark link label, and
// folds newlines because link text cannot span lines. A filename is whatever
// the client sent, so a name like `frame[0].png` has to leave here escaped or
// the link breaks wherever it is pasted. It mirrors the CLI's own escaper, and
// the real registry's.
var labelEscaper = strings.NewReplacer(`\`, `\\`, `[`, `\[`, `]`, `\]`, "\n", " ", "\r", " ")

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

// Lowercase base36, because a slug has to be legal as a DNS label: an artifact
// is hosted at art-{slug}.krowkusercontent.com, and DNS labels are
// case-insensitive, so a case-sensitive alphabet would collide there.
const slugAlphabet = "0123456789abcdefghijklmnopqrstuvwxyz"

// slugRandomLength is canon's slug shape: the type prefix plus exactly this
// many random characters. Pinned by TestMintedSlugsHaveTheCanonicalShape,
// because readers validate it — the website refuses /^art_[a-z0-9]{24}$/
// misses before it ever calls the registry, so a stand-in minting a different
// shape hands out slugs a dev site will not accept.
const slugRandomLength = 24

func generateSlug(prefix string) string {
	b := make([]byte, slugRandomLength)
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

// unreadableBody answers the body that never parsed, and reports whether it did.
//
// Split out from the missing-parameter refusals it sits next to because the two
// are different failures with different fixes: a body the registry could not
// read has nothing to name a parameter from, so saying one is absent sends a
// client looking for a field when the whole payload is broken. The real
// registry raises this out of Rails' parser, before an action reads a single
// parameter, and answers `bad_request` for it.
//
// A truncated body is io.ErrUnexpectedEOF rather than a SyntaxError, and it is
// the likelier of the two in the wild — a connection that dropped mid-write.
// An empty body is neither, and stays the caller's own handler's business.
func unreadableBody(w http.ResponseWriter, err error) bool {
	var syntax *json.SyntaxError
	if !errors.As(err, &syntax) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return false
	}

	writeError(w, http.StatusBadRequest, "bad_request", "The request body is not valid JSON.", nil)
	return true
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
	_ = encodeJSON(w, status, body)
}

// encodeJSON is writeJSON for the one caller that has to know whether the bytes
// went out: handing over a one-shot key is only a delivery if the response
// actually left.
func encodeJSON(w http.ResponseWriter, status int, body any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(body)
}

// writeXMLError is object storage's error shape, not the registry's.
func writeXMLError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<Error><Code>%s</Code></Error>", code)
}
