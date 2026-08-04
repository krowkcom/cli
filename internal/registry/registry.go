// Package registry is a stand-in for api.krowk.com until the real one exists.
// It implements the contract the CLI speaks — the three-step upload handshake,
// idempotency keys verified against the bytes that actually arrive, machine
// readable errors, rate-limit headers — so the CLI can be developed and demoed
// against something honest, and so the real registry has a spec to satisfy.
package registry

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultLimitBytes matches the free tier the website advertises.
	DefaultLimitBytes int64 = 100 << 20
	dailyUploads            = 100
	// workspaceExpiry is how long a link lives once it belongs to a workspace.
	workspaceExpiry = 48 * time.Hour
	// anonymousExpiry is shorter, because nobody has claimed the upload yet.
	// Claiming it moves it onto the workspace window.
	anonymousExpiry = 24 * time.Hour
)

type file struct {
	Filename    string `json:"filename"`
	Bytes       int64  `json:"bytes"`
	ContentType string `json:"content_type"`
}

type artifact struct {
	ID         string          `json:"id"`
	URL        string          `json:"url"`
	PreviewURL string          `json:"preview_url"`
	Bytes      int64           `json:"bytes"`
	ExpiresAt  string          `json:"expires_at"`
	Files      []file          `json:"files"`
	Metadata   json.RawMessage `json:"metadata"`
	// Anonymous means no key was presented, so this upload belongs to nobody
	// yet and expires sooner.
	Anonymous bool `json:"anonymous,omitempty"`
	// ClaimURL adopts an anonymous upload into a workspace. It is a website
	// URL, opened signed-in in a browser — not an API call — and it is a
	// capability, so it is never part of the paste-ready output. It is minted
	// into the finalize response only, never stored.
	ClaimURL string `json:"claim_url,omitempty"`
}

// manifestFile is one declared file, before its bytes arrive.
type manifestFile struct {
	Filename    string `json:"filename"`
	Bytes       int64  `json:"bytes"`
	ContentType string `json:"content_type"`
	Digest      string `json:"digest"`
}

type beginRequest struct {
	IdempotencyKey string          `json:"idempotency_key"`
	Files          []manifestFile  `json:"files"`
	Metadata       json.RawMessage `json:"metadata"`
}

type uploadTarget struct {
	Filename string            `json:"filename"`
	Method   string            `json:"method"`
	URL      string            `json:"url"`
	Headers  map[string]string `json:"headers,omitempty"`
}

type beginResponse struct {
	ID          string         `json:"id"`
	Uploads     []uploadTarget `json:"uploads,omitempty"`
	FinalizeURL string         `json:"finalize_url,omitempty"`
	Complete    bool           `json:"complete,omitempty"`
	Artifact    *artifact      `json:"artifact,omitempty"`
}

// slot is one declared file plus whatever has arrived for it.
type slot struct {
	manifestFile
	token    string
	received bool
}

// upload is an in-flight handshake, keyed by its scoped idempotency key.
type upload struct {
	id  string
	key string
	// scoped is the key qualified by ownership class — the form the store's
	// maps use, so uploads in different classes never deduplicate together.
	scoped   string
	metadata json.RawMessage
	slots    []*slot
	// anonymous is decided when the handshake opens, so an upload cannot change
	// hands halfway through by finalizing with a different key.
	anonymous bool
}

type store struct {
	mu        sync.Mutex
	pending   map[string]*upload  // by scoped idempotency key
	byID      map[string]*upload  // by artifact ID, for finalize
	byToken   map[string]tokenRef // by blob token, for PUT
	artifacts map[string]artifact // finalized, by artifact ID
	finalized map[string]string   // scoped idempotency key -> artifact ID
	// idOwner remembers which scoped key claimed each ID, pending or finalized,
	// so a second upload can never be handed an ID that is already spoken for.
	idOwner map[string]string // artifact ID -> scoped idempotency key
}

type tokenRef struct {
	upload *upload
	index  int
}

// Handler serves the mock registry. siteURL is the origin baked into returned
// links; empty means "whatever host the request arrived on".
func Handler(limitBytes int64, siteURL string) http.Handler {
	if limitBytes <= 0 {
		limitBytes = DefaultLimitBytes
	}

	s := &store{
		pending:   map[string]*upload{},
		byID:      map[string]*upload{},
		byToken:   map[string]tokenRef{},
		artifacts: map[string]artifact{},
		finalized: map[string]string{},
		idOwner:   map[string]string{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/keys/verify", s.verify)
	mux.HandleFunc("POST /v1/artifacts", s.begin(limitBytes, siteURL))
	mux.HandleFunc("PUT /v1/blobs/{token}", s.put(limitBytes))
	mux.HandleFunc("POST /v1/artifacts/{id}/finalize", s.finalize(siteURL))
	mux.HandleFunc("GET /v1/artifacts/{id}", s.get)
	mux.HandleFunc("/", notFound)

	return mux
}

// keyInfo is what the registry knows about one API key.
type keyInfo struct {
	Valid     bool     `json:"valid"`
	KeyID     string   `json:"key_id,omitempty"`
	Workspace string   `json:"workspace,omitempty"`
	Scopes    []string `json:"scopes,omitempty"`
}

// KeyPrefix is the shape the platform issues. The mock has no issuing service,
// so the token's own prefix stands in for a lookup: krk_ro_ is read-only,
// anything else with the krk_ prefix can write. Enough to exercise the CLI's
// scope handling without inventing a key database.
const (
	KeyPrefix         = "krk_"
	readOnlyKeyPrefix = "krk_ro_"
)

// bearer pulls the token out of the request, if there is one.
func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if after, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(after)
	}
	return ""
}

// describeKey resolves a token to what it may do. An empty token is anonymous,
// which is a valid way to upload, not a rejected key.
func describeKey(token string) (info keyInfo, anonymous bool) {
	if token == "" {
		return keyInfo{}, true
	}
	if !strings.HasPrefix(token, KeyPrefix) {
		return keyInfo{Valid: false}, false
	}
	scopes := []string{"artifacts:read", "artifacts:write"}
	if strings.HasPrefix(token, readOnlyKeyPrefix) {
		scopes = []string{"artifacts:read"}
	}
	// Derived, never the token itself — a key ID ends up in logs and output.
	sum := sha256.Sum256([]byte(token))
	return keyInfo{
		Valid:     true,
		KeyID:     "key_" + hex.EncodeToString(sum[:])[:8],
		Workspace: "acme",
		Scopes:    scopes,
	}, false
}

// anonymousClass is the ownership class of an upload made without a key.
const anonymousClass = "anonymous"

// ownerClass names the ownership partition a request uploads into: anonymous,
// or a workspace. Dedup happens only within one class — the idempotency key is
// derived from the bytes, so without the partition a keyed push of a file
// someone had already pushed anonymously would inherit their unowned artifact,
// with its shorter expiry and no way to adopt it into the workspace.
//
// Every handler that classifies calls authorize first, which turns an invalid
// token into a 401 — so the invalid branch here is defence in depth, never a
// silent fall into the anonymous class.
func ownerClass(r *http.Request) string {
	info, anonymous := describeKey(bearer(r))
	if anonymous || !info.Valid {
		return anonymousClass
	}
	return "workspace\x00" + info.Workspace
}

// scopedKey is the idempotency key qualified by its ownership class. Every map
// keyed by idempotency key uses this form, so uploads in different classes can
// never observe each other.
func scopedKey(class, key string) string {
	return class + "\x00" + key
}

// verify lets the CLI self-check auth and scopes before doing any work.
func (s *store) verify(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	rate := rateHeaders(max(0, dailyUploads-len(s.artifacts)))
	s.mu.Unlock()

	info, anonymous := describeKey(bearer(r))
	if anonymous {
		writeJSON(w, http.StatusUnauthorized, rate, map[string]any{
			"error":     "no_key",
			"fix":       "send `Authorization: Bearer krk_...`, or upload anonymously and claim it later",
			"retryable": false,
		})
		return
	}
	if !info.Valid {
		writeJSON(w, http.StatusUnauthorized, rate, map[string]any{
			"error":     "invalid_key",
			"fix":       "this is not a krowk API key — they start with " + KeyPrefix,
			"retryable": false,
		})
		return
	}
	writeJSON(w, http.StatusOK, rate, info)
}

// authorize enforces the write scope on the upload path.
func authorize(w http.ResponseWriter, r *http.Request, rate map[string]string) bool {
	info, anonymous := describeKey(bearer(r))
	if anonymous {
		return true // anonymous uploads are allowed
	}
	if !info.Valid {
		writeJSON(w, http.StatusUnauthorized, rate, map[string]any{
			"error":     "invalid_key",
			"fix":       "this is not a krowk API key — they start with " + KeyPrefix,
			"retryable": false,
		})
		return false
	}
	if !slices.Contains(info.Scopes, "artifacts:write") {
		writeJSON(w, http.StatusForbidden, rate, map[string]any{
			"error":     "insufficient_scope",
			"required":  "artifacts:write",
			"scopes":    info.Scopes,
			"fix":       "this key may only read — push with a key that carries artifacts:write",
			"retryable": false,
		})
		return false
	}
	return true
}

// begin takes the manifest and hands back one presigned target per file.
func (s *store) begin(limitBytes int64, siteURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req beginRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, nil, map[string]any{
				"error":     "malformed_manifest",
				"detail":    err.Error(),
				"fix":       "POST a JSON body with `idempotency_key` and a `files` array",
				"retryable": false,
			})
			return
		}
		if req.IdempotencyKey == "" {
			writeJSON(w, http.StatusUnprocessableEntity, nil, map[string]any{
				"error":     "missing_idempotency_key",
				"fix":       "send `idempotency_key`: the digest of the files, so a retry is free",
				"retryable": false,
			})
			return
		}
		if len(req.Files) == 0 {
			writeJSON(w, http.StatusUnprocessableEntity, nil, map[string]any{
				"error":     "no_file",
				"fix":       "declare at least one file in `files`",
				"retryable": false,
			})
			return
		}

		var total int64
		for _, f := range req.Files {
			if f.Filename == "" || f.Digest == "" || f.Bytes < 0 {
				writeJSON(w, http.StatusUnprocessableEntity, nil, map[string]any{
					"error":     "malformed_manifest",
					"fix":       "every file needs `filename`, `bytes` and `digest`",
					"retryable": false,
				})
				return
			}
			total += f.Bytes
		}

		s.mu.Lock()
		defer s.mu.Unlock()

		rate := rateHeaders(max(0, dailyUploads-len(s.artifacts)))

		// Auth before any work, so a key that cannot write finds out here
		// rather than after streaming a 2 GB video to storage.
		if !authorize(w, r, rate) {
			return
		}

		// The same key twice, in the same ownership class: hand back the
		// finished artifact rather than a second set of upload targets. This is
		// what makes a retry free. The class matters — the key comes from the
		// bytes, so without it a keyed push would inherit an anonymous upload
		// of the same file instead of minting one the workspace owns. The
		// stored artifact never carries the claim URL — see finalize.
		class := ownerClass(r)
		scoped := scopedKey(class, req.IdempotencyKey)
		if id, ok := s.finalized[scoped]; ok {
			done := s.artifacts[id]
			writeJSON(w, http.StatusOK, rate, beginResponse{ID: id, Complete: true, Artifact: &done})
			return
		}

		if total > limitBytes {
			writeJSON(w, http.StatusRequestEntityTooLarge, rate, map[string]any{
				"error":       "artifact_too_large",
				"limit_bytes": limitBytes,
				"got_bytes":   total,
				"fix":         fmt.Sprintf("re-encode below %d MB or push frames separately", limitBytes>>20),
				"retryable":   false,
			})
			return
		}

		// An interrupted handshake resumes: same key, same targets, so the
		// blobs already stored stay stored.
		up, resumed := s.pending[scoped]
		if !resumed {
			up = &upload{
				id:        s.mintID(scoped),
				key:       req.IdempotencyKey,
				scoped:    scoped,
				metadata:  validMetadata(req.Metadata),
				slots:     make([]*slot, 0, len(req.Files)),
				anonymous: class == anonymousClass,
			}
			s.idOwner[up.id] = up.scoped
			for _, f := range req.Files {
				sl := &slot{manifestFile: f, token: newToken()}
				up.slots = append(up.slots, sl)
				s.byToken[sl.token] = tokenRef{upload: up, index: len(up.slots) - 1}
			}
			s.pending[up.scoped] = up
			s.byID[up.id] = up
		}

		// Endpoints the client calls back into are always on the host the
		// request arrived at — siteURL rebrands the public links only, and the
		// client rightly refuses to take its token to a different origin.
		api := requestOrigin(r)
		targets := make([]uploadTarget, 0, len(up.slots))
		for _, sl := range up.slots {
			targets = append(targets, uploadTarget{
				Filename: sl.Filename,
				Method:   http.MethodPut,
				// A real registry points this at object storage. Serving it
				// ourselves keeps the mock a single process while exercising
				// exactly the same client path.
				URL:     fmt.Sprintf("%s/v1/blobs/%s", api, sl.token),
				Headers: map[string]string{"Content-Type": sl.ContentType},
			})
		}
		writeJSON(w, http.StatusCreated, rate, beginResponse{
			ID:          up.id,
			Uploads:     targets,
			FinalizeURL: fmt.Sprintf("%s/v1/artifacts/%s/finalize", api, up.id),
		})
	}
}

// put stands in for the presigned storage endpoint. It verifies the bytes
// against what the manifest promised, which is what lets finalize trust the
// idempotency key.
func (s *store) put(limitBytes int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		ref, ok := s.byToken[r.PathValue("token")]
		s.mu.Unlock()
		if !ok {
			writeJSON(w, http.StatusForbidden, nil, map[string]any{
				"error":     "upload_url_unknown",
				"fix":       "the presigned URL is expired or was never issued — start the handshake again",
				"retryable": false,
			})
			return
		}
		sl := ref.upload.slots[ref.index]

		sum := sha256.New()
		// One byte past the declared size is enough to catch an overrun without
		// reading an unbounded body.
		written, err := io.Copy(sum, io.LimitReader(r.Body, sl.Bytes+1))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, nil, map[string]any{
				"error": "upload_unreadable", "detail": err.Error(), "retryable": true,
			})
			return
		}
		if written > limitBytes {
			writeJSON(w, http.StatusRequestEntityTooLarge, nil, map[string]any{
				"error":       "artifact_too_large",
				"limit_bytes": limitBytes,
				"got_bytes":   written,
				"fix":         fmt.Sprintf("re-encode below %d MB or push frames separately", limitBytes>>20),
				"retryable":   false,
			})
			return
		}
		if written != sl.Bytes {
			writeJSON(w, http.StatusUnprocessableEntity, nil, map[string]any{
				"error":          "size_mismatch",
				"declared_bytes": sl.Bytes,
				"got_bytes":      written,
				"fix":            "the body length must match the `bytes` declared for " + sl.Filename,
				"retryable":      false,
			})
			return
		}
		if got := hex.EncodeToString(sum.Sum(nil)); got != sl.Digest {
			writeJSON(w, http.StatusUnprocessableEntity, nil, map[string]any{
				"error":           "digest_mismatch",
				"declared_digest": sl.Digest,
				"got_digest":      got,
				"fix":             "the bytes sent for " + sl.Filename + " are not the ones declared in the manifest",
				"retryable":       false,
			})
			return
		}

		s.mu.Lock()
		sl.received = true
		s.mu.Unlock()

		w.WriteHeader(http.StatusOK)
	}
}

// finalize turns a complete set of blobs into an artifact.
func (s *store) finalize(siteURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			IdempotencyKey string `json:"idempotency_key"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body)

		id := r.PathValue("id")

		s.mu.Lock()
		defer s.mu.Unlock()

		rate := rateHeaders(max(0, dailyUploads-len(s.artifacts)))

		// The same gate as begin: an invalid token is a 401, not a silent fall
		// into the anonymous class, and a read-only key cannot complete what it
		// could never have opened. Without this, finalize would classify
		// `Bearer garbage` as anonymous and let a krk_ro_ key — same workspace
		// class as a writer — finish a pending workspace upload.
		if !authorize(w, r, rate) {
			return
		}

		// The ID is public — it is the last segment of the link people paste
		// into pull requests. So it authorises nothing on its own: proving you
		// opened this handshake means presenting the key, which is derived from
		// the bytes and never leaves the pusher.
		if body.IdempotencyKey == "" {
			writeJSON(w, http.StatusUnprocessableEntity, rate, map[string]any{
				"error":     "missing_idempotency_key",
				"fix":       "finalize with the same idempotency_key the handshake was opened with",
				"retryable": false,
			})
			return
		}

		// Finalizing twice is not an error; it is the retry path. But only for
		// the caller that opened it — the key and the ownership class must both
		// agree, so a keyed caller holding the same bytes as an anonymous upload
		// gets the same answer as for an ID that was never theirs. The stored
		// artifact never carries the claim URL, so a replay cannot leak it.
		if done, ok := s.artifacts[id]; ok {
			if s.finalized[scopedKey(ownerClass(r), body.IdempotencyKey)] != id {
				writeJSON(w, http.StatusConflict, rate, map[string]any{
					"error":     "idempotency_key_mismatch",
					"fix":       "finalize with the same idempotency_key the handshake was opened with",
					"retryable": false,
				})
				return
			}
			writeJSON(w, http.StatusOK, rate, done)
			return
		}

		up, ok := s.byID[id]
		if !ok {
			writeJSON(w, http.StatusNotFound, rate, map[string]any{
				"error":     "upload_not_found",
				"id":        id,
				"fix":       "no handshake is open for this ID — POST /v1/artifacts first",
				"retryable": false,
			})
			return
		}
		// Pending uploads are partitioned the same way as finished ones: the key
		// alone is derivable from the bytes, so a keyed caller holding a copy of
		// an anonymously-pushed file could otherwise finalize the anonymous
		// pending upload and walk off with its one-time claim capability.
		if scopedKey(ownerClass(r), body.IdempotencyKey) != up.scoped {
			writeJSON(w, http.StatusConflict, rate, map[string]any{
				"error":     "idempotency_key_mismatch",
				"fix":       "finalize with the same idempotency_key the handshake was opened with",
				"retryable": false,
			})
			return
		}

		var total int64
		files := make([]file, 0, len(up.slots))
		for _, sl := range up.slots {
			if !sl.received {
				writeJSON(w, http.StatusConflict, rate, map[string]any{
					"error":     "upload_incomplete",
					"missing":   sl.Filename,
					"fix":       "PUT every file's bytes to its presigned URL before finalizing",
					"retryable": false,
				})
				return
			}
			total += sl.Bytes
			files = append(files, file{
				Filename:    sl.Filename,
				Bytes:       sl.Bytes,
				ContentType: contentType(sl.ContentType),
			})
		}

		// The key is the fold of what was declared, and every blob was checked
		// against its declared digest on arrival — so agreeing here means the
		// key really does identify the bytes that were stored.
		if got := foldKey(up.slots); got != up.key {
			writeJSON(w, http.StatusUnprocessableEntity, rate, map[string]any{
				"error":     "idempotency_key_mismatch",
				"expected":  got,
				"got":       up.key,
				"fix":       "derive idempotency_key from each file's name, size and digest in manifest order",
				"retryable": false,
			})
			return
		}

		site := publicOrigin(r, siteURL)
		window := workspaceExpiry
		if up.anonymous {
			window = anonymousExpiry
		}
		a := artifact{
			ID:         up.id,
			URL:        fmt.Sprintf("%s/a/%s", site, up.id),
			PreviewURL: fmt.Sprintf("%s/a/%s/preview.png", site, up.id),
			Bytes:      total,
			ExpiresAt:  time.Now().Add(window).UTC().Format(time.RFC3339),
			Files:      files,
			Metadata:   up.metadata,
			Anonymous:  up.anonymous,
		}
		s.artifacts[a.ID] = a
		s.finalized[up.scoped] = a.ID
		delete(s.pending, up.scoped)
		for _, sl := range up.slots {
			delete(s.byToken, sl.token)
		}

		if up.anonymous {
			// Whoever holds this can adopt the upload, so it is minted straight
			// into this one response — never stored — and handed back exactly
			// once, on the single response that finalizes the artifact. No later
			// egress path can leak what the store never held.
			//
			// "Once" is a promise about responses, not people: completing the
			// upload earns the claim. Anonymous identity is derived from the
			// bytes, so if the opener is interrupted before finalizing, a second
			// anonymous pusher of the same file resumes the pending handshake
			// and — by finalizing first — receives the claim URL and the
			// opener's metadata. There is no opener identity to check against;
			// the alternatives (fresh identity per anonymous begin, or expiring
			// pending uploads) would break the resumability and dedup this
			// registry promises, so the trade-off is made deliberately and
			// stated in the README.
			a.ClaimURL = fmt.Sprintf("%s/claim/%s", site, newToken())
		}

		// Recomputed now this upload is counted, so the header an agent reads
		// off a successful push is its remaining quota, not its previous one.
		writeJSON(w, http.StatusOK, rateHeaders(max(0, dailyUploads-len(s.artifacts))), a)
	}
}

func (s *store) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	s.mu.Lock()
	a, ok := s.artifacts[id]
	s.mu.Unlock()

	if !ok {
		writeJSON(w, http.StatusNotFound, nil, map[string]any{
			"error":     "artifact_not_found",
			"id":        id,
			"fix":       "check the ID",
			"retryable": false,
		})
		return
	}
	// The ID is public — it is in the shareable link. The claim URL is not; it
	// was minted into the finalize response and never stored, so a lookup has
	// nothing to leak.
	writeJSON(w, http.StatusOK, nil, a)
}

func notFound(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotFound, nil, map[string]any{
		"error":     "not_found",
		"fix":       "POST " + requestOrigin(r) + "/v1/artifacts with a file manifest",
		"retryable": false,
	})
}

// foldKey recomputes the client's idempotency key from the manifest.
func foldKey(slots []*slot) string {
	h := sha256.New()
	for _, sl := range slots {
		fmt.Fprintf(h, "%s\x00%d\x00%s\x00", sl.Filename, sl.Bytes, sl.Digest)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// idLength is how many hex characters a public ID starts at. Short, because it
// ends up in every link that gets pasted anywhere.
const idLength = 7

// mintID derives the public ID from the scoped idempotency key, so the same
// bytes pushed by the same ownership class always resolve to the same link
// without the key itself appearing in the URL.
//
// Seven hex characters is only 28 bits, which sounds like plenty and is not: two
// unrelated uploads collide after a few thousand, well inside a real registry's
// first week. So a collision lengthens the ID rather than letting a new upload
// take over an existing one's link. IDs stay short until they cannot be.
func (s *store) mintID(key string) string {
	full := idDigest(key)
	for n := idLength; n < len(full); n++ {
		candidate := full[:n]
		if owner, taken := s.idOwner[candidate]; !taken || owner == key {
			return candidate
		}
	}
	return full
}

func idDigest(key string) string {
	sum := sha256.Sum256([]byte("krowk-artifact\x00" + key))
	return hex.EncodeToString(sum[:])
}

func newToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice; a panic here beats issuing a
		// predictable upload URL.
		panic("registry: out of randomness: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

func validMetadata(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || !json.Valid(raw) {
		return json.RawMessage("{}")
	}
	return raw
}

func rateHeaders(remaining int) map[string]string {
	return map[string]string{
		"X-RateLimit-Limit":     strconv.Itoa(dailyUploads),
		"X-RateLimit-Remaining": strconv.Itoa(remaining),
	}
}

func writeJSON(w http.ResponseWriter, status int, headers map[string]string, body any) {
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}

// publicOrigin is the origin baked into shareable links.
func publicOrigin(r *http.Request, siteURL string) string {
	if siteURL != "" {
		return siteURL
	}
	return requestOrigin(r)
}

// requestOrigin is where this mock is actually listening.
func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func contentType(s string) string {
	if s == "" {
		return "application/octet-stream"
	}
	return s
}
