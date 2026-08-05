package registry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// declare builds the manifest and the idempotency key the CLI would send.
func declare(files ...[2]string) beginRequest {
	key := sha256.New()
	req := beginRequest{Metadata: json.RawMessage(`{"repo":"acme/storefront"}`)}
	for _, f := range files {
		name, body := f[0], f[1]
		sum := sha256.Sum256([]byte(body))
		digest := hex.EncodeToString(sum[:])
		fmt.Fprintf(key, "%s\x00%d\x00%s\x00", name, len(body), digest)
		req.Files = append(req.Files, manifestFile{
			Filename:    name,
			Bytes:       int64(len(body)),
			ContentType: "image/png",
			Digest:      digest,
		})
	}
	req.IdempotencyKey = hex.EncodeToString(key.Sum(nil))
	return req
}

type client struct {
	t   *testing.T
	url string
}

func serve(t *testing.T, limit int64) *client {
	t.Helper()
	c, _ := serveStore(t, limit)
	return c
}

// serveStore also hands back the store, for the assertions that are about state
// being reclaimed rather than about a response.
func serveStore(t *testing.T, limit int64) (*client, *store) {
	t.Helper()
	s := newStore()
	srv := httptest.NewServer(s.handler(limit, ""))
	t.Cleanup(srv.Close)
	return &client{t: t, url: srv.URL}, s
}

func (c *client) post(path string, body any) (int, map[string]any) {
	c.t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		c.t.Fatal(err)
	}
	res, err := http.Post(c.url+path, "application/json", bytes.NewReader(payload))
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

// postURL posts to an absolute URL, as the client does with finalize_url.
func (c *client) postURL(url string, body any) (int, map[string]any) {
	c.t.Helper()
	return c.post(strings.TrimPrefix(url, c.url), body)
}

func (c *client) put(url, body string) (int, map[string]any) {
	c.t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "image/png")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	out := map[string]any{}
	payload, _ := io.ReadAll(res.Body)
	_ = json.Unmarshal(payload, &out)
	return res.StatusCode, out
}

// begin returns the upload targets and the finalize URL.
func (c *client) begin(req beginRequest) (int, beginResponse) {
	c.t.Helper()
	payload, err := json.Marshal(req)
	if err != nil {
		c.t.Fatal(err)
	}
	res, err := http.Post(c.url+"/v1/artifacts", "application/json", bytes.NewReader(payload))
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	var out beginResponse
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

// beginAs opens a handshake with an Authorization header.
func (c *client) beginAs(token string, req beginRequest) (int, beginResponse) {
	c.t.Helper()
	payload, err := json.Marshal(req)
	if err != nil {
		c.t.Fatal(err)
	}
	post, err := http.NewRequest(http.MethodPost, c.url+"/v1/artifacts", bytes.NewReader(payload))
	if err != nil {
		c.t.Fatal(err)
	}
	post.Header.Set("Content-Type", "application/json")
	if token != "" {
		post.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(post)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	var out beginResponse
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestHandshakeStoresTheArtifactAndIsIdempotent(t *testing.T) {
	c := serve(t, 0)
	req := declare([2]string{"before.png", "one"}, [2]string{"after.png", "two"})

	status, begun := c.begin(req)
	if status != http.StatusCreated {
		t.Fatalf("begin = %d, want 201", status)
	}
	if len(begun.Uploads) != 2 || begun.FinalizeURL == "" {
		t.Fatalf("begin returned %+v", begun)
	}

	for i, body := range []string{"one", "two"} {
		if code, out := c.put(begun.Uploads[i].URL, body); code != http.StatusOK {
			t.Fatalf("put %d = %d: %v", i, code, out)
		}
	}

	code, done := c.postURL(begun.FinalizeURL, map[string]string{"idempotency_key": req.IdempotencyKey})
	if code != http.StatusOK {
		t.Fatalf("finalize = %d: %v", code, done)
	}
	if done["bytes"] != float64(6) {
		t.Errorf("bytes = %v, want 6", done["bytes"])
	}
	if url, _ := done["url"].(string); !strings.HasSuffix(url, "/a/"+begun.ID) {
		t.Errorf("url = %q, want it to end in the ID handed out at begin", url)
	}
	meta, _ := done["metadata"].(map[string]any)
	if meta["repo"] != "acme/storefront" {
		t.Errorf("metadata = %v, want it round-tripped", done["metadata"])
	}

	// The same key again short-circuits: no second set of upload targets, and
	// the same link. This is the retry path an agent will actually hit.
	status, again := c.begin(req)
	if status != http.StatusOK || !again.Complete || again.Artifact == nil {
		t.Fatalf("replay = %d %+v, want 200 and complete", status, again)
	}
	if again.Artifact.ID != begun.ID {
		t.Errorf("replay produced %q, want %q", again.Artifact.ID, begun.ID)
	}
	if len(again.Uploads) != 0 {
		t.Errorf("replay handed out %d upload target(s)", len(again.Uploads))
	}
}

func TestInterruptedHandshakeResumesOntoTheSameTargets(t *testing.T) {
	c := serve(t, 0)
	req := declare([2]string{"shot.png", "one"})

	_, first := c.begin(req)
	// The process dies before finalizing and the whole command runs again.
	_, second := c.begin(req)

	if first.ID != second.ID || first.Uploads[0].URL != second.Uploads[0].URL {
		t.Errorf("resume changed the targets: %+v then %+v", first, second)
	}
}

// A resume restarts the expiry clock. If it kept the original declaration's
// timestamp, a handshake resumed just under the TTL would be swept by the next
// unrelated declaration and its finalize would 404 — the client would have to
// resend every byte, which is exactly what "resumable" promises not to do.
func TestResumedHandshakeSurvivesASweep(t *testing.T) {
	c, s := serveStore(t, 0)
	req := declare([2]string{"shot.png", "one"})

	_, first := c.begin(req)

	// Age the handshake to just under the TTL, then resume it.
	s.mu.Lock()
	s.pending[scopedKey(anonymousClass, req.IdempotencyKey)].at = s.pending[scopedKey(anonymousClass, req.IdempotencyKey)].at.Add(-pendingTTL + time.Minute)
	s.mu.Unlock()
	if code, second := c.begin(req); code != http.StatusCreated || second.ID != first.ID {
		t.Fatalf("resume = %d id %q, want 201 with id %q", code, second.ID, first.ID)
	}

	// Two more minutes pass — past the original declaration's TTL, well within
	// the resume's — and an unrelated declaration triggers a sweep.
	s.mu.Lock()
	s.pending[scopedKey(anonymousClass, req.IdempotencyKey)].at = s.pending[scopedKey(anonymousClass, req.IdempotencyKey)].at.Add(-2 * time.Minute)
	s.mu.Unlock()
	if code, _ := c.begin(declare([2]string{"other.png", "two"})); code != http.StatusCreated {
		t.Fatal("unrelated declaration failed")
	}

	// The resumed handshake survives and finalizes.
	if code, _ := c.put(first.Uploads[0].URL, "one"); code != http.StatusOK {
		t.Fatalf("put after the sweep = %d, want 200 — the handshake was reaped", code)
	}
	code, out := c.postURL(first.FinalizeURL, map[string]string{"idempotency_key": req.IdempotencyKey})
	if code != http.StatusOK {
		t.Fatalf("finalize after the sweep = %d %v, want 200", code, out)
	}
}

// A transfer slower than the TTL never re-declares, so each landed blob must
// keep the handshake alive on its own.
func TestActivelyUploadingHandshakeSurvivesASweep(t *testing.T) {
	c, s := serveStore(t, 0)
	req := declare([2]string{"a.png", "one"}, [2]string{"b.png", "two"})

	_, begun := c.begin(req)

	// The first blob lands just under the TTL after the declaration.
	s.mu.Lock()
	s.pending[scopedKey(anonymousClass, req.IdempotencyKey)].at = s.pending[scopedKey(anonymousClass, req.IdempotencyKey)].at.Add(-pendingTTL + time.Minute)
	s.mu.Unlock()
	if code, _ := c.put(begun.Uploads[0].URL, "one"); code != http.StatusOK {
		t.Fatal("first put failed")
	}

	// Two more minutes on, an unrelated declaration sweeps. The slow transfer's
	// last PUT reset the clock, so it must not be reaped mid-flight.
	s.mu.Lock()
	s.pending[scopedKey(anonymousClass, req.IdempotencyKey)].at = s.pending[scopedKey(anonymousClass, req.IdempotencyKey)].at.Add(-2 * time.Minute)
	s.mu.Unlock()
	if code, _ := c.begin(declare([2]string{"other.png", "three"})); code != http.StatusCreated {
		t.Fatal("unrelated declaration failed")
	}

	if code, _ := c.put(begun.Uploads[1].URL, "two"); code != http.StatusOK {
		t.Fatalf("second put after the sweep = %d, want 200 — the handshake was reaped", code)
	}
	code, out := c.postURL(begun.FinalizeURL, map[string]string{"idempotency_key": req.IdempotencyKey})
	if code != http.StatusOK {
		t.Fatalf("finalize after the sweep = %d %v, want 200", code, out)
	}
}

func TestBytesThatDoNotMatchTheManifestAreRejected(t *testing.T) {
	c := serve(t, 0)
	req := declare([2]string{"shot.png", "one"})
	_, begun := c.begin(req)

	// Same length, different bytes: only the digest catches this.
	code, out := c.put(begun.Uploads[0].URL, "two")
	if code != http.StatusUnprocessableEntity || out["error"] != "digest_mismatch" {
		t.Fatalf("put = %d %v, want 422 digest_mismatch", code, out)
	}

	code, out = c.put(begun.Uploads[0].URL, "one and a half")
	if code != http.StatusUnprocessableEntity || out["error"] != "size_mismatch" {
		t.Fatalf("put = %d %v, want 422 size_mismatch", code, out)
	}
}

func TestFinalizeNeedsEveryBlob(t *testing.T) {
	c := serve(t, 0)
	req := declare([2]string{"before.png", "one"}, [2]string{"after.png", "two"})
	_, begun := c.begin(req)

	if code, _ := c.put(begun.Uploads[0].URL, "one"); code != http.StatusOK {
		t.Fatal("first put failed")
	}

	code, out := c.postURL(begun.FinalizeURL, map[string]string{"idempotency_key": req.IdempotencyKey})
	if code != http.StatusConflict || out["error"] != "upload_incomplete" {
		t.Fatalf("finalize = %d %v, want 409 upload_incomplete", code, out)
	}
	if out["missing"] != "after.png" {
		t.Errorf("missing = %v, want the file that never arrived", out["missing"])
	}
}

// The artifact ID is public — it is the last segment of every shared link — so
// it must authorise nothing. Without the key, finalize is a lookup that hands
// out whatever the response carries.
func TestFinalizeWillNotHandTheArtifactToTheIDAlone(t *testing.T) {
	c := serve(t, 0)
	req := declare([2]string{"shot.png", "one"})
	_, begun := c.begin(req)
	if code, _ := c.put(begun.Uploads[0].URL, "one"); code != http.StatusOK {
		t.Fatal("put failed")
	}
	if code, _ := c.postURL(begun.FinalizeURL, map[string]string{"idempotency_key": req.IdempotencyKey}); code != http.StatusOK {
		t.Fatal("finalize failed")
	}

	// A stranger with the shared link, and nothing else.
	code, out := c.postURL(begun.FinalizeURL, map[string]any{})
	if code == http.StatusOK {
		t.Fatalf("the ID alone returned the artifact: %v", out)
	}
	if out["error"] != "missing_idempotency_key" {
		t.Errorf("error = %v, want missing_idempotency_key", out["error"])
	}

	// And a wrong key does not work either.
	code, out = c.postURL(begun.FinalizeURL, map[string]string{"idempotency_key": strings.Repeat("b", 64)})
	if code != http.StatusConflict || out["error"] != "idempotency_key_mismatch" {
		t.Errorf("wrong key = %d %v, want 409 idempotency_key_mismatch", code, out)
	}
}

// A stranger who guesses a pending upload's ID must not be able to complete it.
func TestFinalizeOfSomeoneElsesPendingUploadIsRefused(t *testing.T) {
	c := serve(t, 0)
	req := declare([2]string{"shot.png", "one"})
	_, begun := c.begin(req)
	if code, _ := c.put(begun.Uploads[0].URL, "one"); code != http.StatusOK {
		t.Fatal("put failed")
	}

	code, out := c.postURL(begun.FinalizeURL, map[string]any{})
	if code == http.StatusOK {
		t.Fatalf("finalized without the key: %v", out)
	}
}

// Seven hex characters collide after a few thousand uploads, which is well
// inside a real registry's first week. A collision must lengthen the ID, never
// hand one upload's link to another.
func TestCollidingIDsDoNotDisplaceEachOther(t *testing.T) {
	s := &store{
		pending:   map[string]*upload{},
		byID:      map[string]*upload{},
		byToken:   map[string]tokenRef{},
		artifacts: map[string]artifact{},
		finalized: map[string]string{},
		idOwner:   map[string]string{},
	}

	keyA := "eb7015b56be919a2c96c3fcb7e4ddd08fc17c1ccf17fd86dd060409ad58e0cb9"
	keyB := "c5adf0a95aa71de9eb2d5e3c0a190e176ded7ecc9642be8744306f4b1175b5b7"
	if idDigest(keyA)[:idLength] != idDigest(keyB)[:idLength] {
		t.Fatal("these keys no longer collide; pick another pair")
	}

	first := s.mintID(keyA)
	s.idOwner[first] = keyA
	second := s.mintID(keyB)

	if len(first) != idLength {
		t.Errorf("the first ID should stay short: %q", first)
	}
	if second == first {
		t.Fatalf("both uploads were given the ID %q", second)
	}
	if len(second) <= idLength {
		t.Errorf("the colliding ID should have lengthened, got %q", second)
	}
	// Still deterministic for the same key.
	if again := s.mintID(keyA); again != first {
		t.Errorf("the same key minted %q then %q", first, again)
	}
}

func TestFinalizeRejectsTheWrongKey(t *testing.T) {
	c := serve(t, 0)
	req := declare([2]string{"shot.png", "one"})
	_, begun := c.begin(req)
	if code, _ := c.put(begun.Uploads[0].URL, "one"); code != http.StatusOK {
		t.Fatal("put failed")
	}

	code, out := c.postURL(begun.FinalizeURL, map[string]string{"idempotency_key": "not-the-key"})
	if code != http.StatusConflict || out["error"] != "idempotency_key_mismatch" {
		t.Fatalf("finalize = %d %v, want 409 idempotency_key_mismatch", code, out)
	}
}

func TestKeyMustDescribeTheManifest(t *testing.T) {
	c := serve(t, 0)
	req := declare([2]string{"shot.png", "one"})
	req.IdempotencyKey = strings.Repeat("a", 64) // well-formed, but not the fold

	_, begun := c.begin(req)
	if code, _ := c.put(begun.Uploads[0].URL, "one"); code != http.StatusOK {
		t.Fatal("put failed")
	}

	code, out := c.postURL(begun.FinalizeURL, map[string]string{"idempotency_key": req.IdempotencyKey})
	if code != http.StatusUnprocessableEntity || out["error"] != "idempotency_key_mismatch" {
		t.Fatalf("finalize = %d %v, want 422 idempotency_key_mismatch", code, out)
	}
}

func TestUnknownUploadTokenIsRefused(t *testing.T) {
	c := serve(t, 0)
	code, out := c.put(c.url+"/v1/blobs/deadbeef", "one")
	if code != http.StatusForbidden || out["error"] != "upload_url_unknown" {
		t.Fatalf("put = %d %v, want 403 upload_url_unknown", code, out)
	}
}

func TestOversizeIsRejectedBeforeAnyBytesMove(t *testing.T) {
	c := serve(t, 2)
	req := declare([2]string{"shot.png", "one"})

	status, out := c.post("/v1/artifacts", req)
	if status != http.StatusRequestEntityTooLarge || out["error"] != "artifact_too_large" {
		t.Fatalf("begin = %d %v, want 413 artifact_too_large", status, out)
	}
	if out["limit_bytes"] != float64(2) || out["got_bytes"] != float64(3) {
		t.Errorf("body = %v, want the limit and the declared total", out)
	}
}

func TestManifestValidation(t *testing.T) {
	c := serve(t, 0)

	if code, out := c.post("/v1/artifacts", map[string]any{"files": []any{}}); code != http.StatusUnprocessableEntity ||
		out["error"] != "missing_idempotency_key" {
		t.Errorf("no key = %d %v", code, out)
	}
	req := declare([2]string{"shot.png", "one"})
	req.Files = nil
	if code, out := c.post("/v1/artifacts", req); code != http.StatusUnprocessableEntity || out["error"] != "no_file" {
		t.Errorf("no files = %d %v", code, out)
	}
	req = declare([2]string{"shot.png", "one"})
	req.Files[0].Digest = ""
	if code, out := c.post("/v1/artifacts", req); code != http.StatusUnprocessableEntity ||
		out["error"] != "malformed_manifest" {
		t.Errorf("no digest = %d %v", code, out)
	}
}

// postAs posts with an Authorization header, so scope handling can be driven.
func (c *client) postAs(path, token string, body any) (int, map[string]any) {
	c.t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		c.t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, c.url+path, bytes.NewReader(payload))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestVerifyReportsScopes(t *testing.T) {
	c := serve(t, 0)

	code, out := c.postAs("/v1/keys/verify", "krk_live_abc", nil)
	if code != http.StatusOK || out["valid"] != true {
		t.Fatalf("verify = %d %v, want 200 valid", code, out)
	}
	if out["workspace"] != "acme" {
		t.Errorf("workspace = %v", out["workspace"])
	}
	scopes, _ := out["scopes"].([]any)
	if len(scopes) != 2 || scopes[1] != "artifacts:write" {
		t.Errorf("scopes = %v, want read and write", out["scopes"])
	}
	// The key ID must be derived, never the token — it ends up in logs.
	if id, _ := out["key_id"].(string); id == "" || strings.Contains(id, "abc") {
		t.Errorf("key_id = %q, want a derived identifier", id)
	}
}

func TestVerifyDistinguishesNoKeyFromBadKey(t *testing.T) {
	c := serve(t, 0)

	if code, out := c.postAs("/v1/keys/verify", "", nil); code != http.StatusUnauthorized || out["error"] != "no_key" {
		t.Errorf("anonymous = %d %v, want 401 no_key", code, out)
	}
	if code, out := c.postAs("/v1/keys/verify", "hunter2", nil); code != http.StatusUnauthorized ||
		out["error"] != "invalid_key" {
		t.Errorf("junk token = %d %v, want 401 invalid_key", code, out)
	}
}

func TestReadOnlyKeyCannotUpload(t *testing.T) {
	c := serve(t, 0)

	code, out := c.postAs("/v1/keys/verify", "krk_ro_abc", nil)
	if code != http.StatusOK {
		t.Fatalf("verify = %d %v", code, out)
	}
	if scopes, _ := out["scopes"].([]any); len(scopes) != 1 || scopes[0] != "artifacts:read" {
		t.Fatalf("scopes = %v, want read only", out["scopes"])
	}

	// Rejected at the manifest, before any bytes are asked for.
	code, out = c.postAs("/v1/artifacts", "krk_ro_abc", declare([2]string{"shot.png", "one"}))
	if code != http.StatusForbidden || out["error"] != "insufficient_scope" {
		t.Fatalf("begin = %d %v, want 403 insufficient_scope", code, out)
	}
	if out["required"] != "artifacts:write" {
		t.Errorf("required = %v, want the missing scope named", out["required"])
	}
}

func TestBadKeyIsRejectedAtTheManifest(t *testing.T) {
	c := serve(t, 0)

	code, out := c.postAs("/v1/artifacts", "hunter2", declare([2]string{"shot.png", "one"}))
	if code != http.StatusUnauthorized || out["error"] != "invalid_key" {
		t.Fatalf("begin = %d %v, want 401 invalid_key", code, out)
	}
}

func TestAnonymousUploadIsAllowed(t *testing.T) {
	c := serve(t, 0)

	code, out := c.postAs("/v1/artifacts", "", declare([2]string{"shot.png", "one"}))
	if code != http.StatusCreated {
		t.Fatalf("anonymous begin = %d %v, want 201", code, out)
	}
}

// pushAs runs the whole handshake and returns the finalized artifact.
func (c *client) pushAs(token string, files ...[2]string) map[string]any {
	c.t.Helper()
	req := declare(files...)

	payload, err := json.Marshal(req)
	if err != nil {
		c.t.Fatal(err)
	}
	post, err := http.NewRequest(http.MethodPost, c.url+"/v1/artifacts", bytes.NewReader(payload))
	if err != nil {
		c.t.Fatal(err)
	}
	post.Header.Set("Content-Type", "application/json")
	if token != "" {
		post.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(post)
	if err != nil {
		c.t.Fatal(err)
	}
	var begun beginResponse
	_ = json.NewDecoder(res.Body).Decode(&begun)
	res.Body.Close()

	// This key was already finalized: no targets, no bytes to send. The client
	// takes the same short-circuit.
	if begun.Complete && begun.Artifact != nil {
		out := map[string]any{}
		encoded, err := json.Marshal(begun.Artifact)
		if err != nil {
			c.t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, &out); err != nil {
			c.t.Fatal(err)
		}
		return out
	}

	for i, f := range files {
		if code, out := c.put(begun.Uploads[i].URL, f[1]); code != http.StatusOK {
			c.t.Fatalf("put %d = %d: %v", i, code, out)
		}
	}
	code, done := c.postAs("/v1/artifacts/"+begun.ID+"/finalize", token,
		map[string]string{"idempotency_key": req.IdempotencyKey})
	if code != http.StatusOK {
		c.t.Fatalf("finalize = %d: %v", code, done)
	}
	return done
}

func TestAnonymousUploadIsEphemeralAndClaimable(t *testing.T) {
	c := serve(t, 0)

	done := c.pushAs("", [2]string{"shot.png", "one"})
	if done["anonymous"] != true {
		t.Errorf("anonymous = %v, want true", done["anonymous"])
	}
	claim, _ := done["claim_url"].(string)
	if !strings.Contains(claim, "/claim/") {
		t.Fatalf("claim_url = %q, want a claim link", claim)
	}

	expires, _ := done["expires_at"].(string)
	at, err := time.Parse(time.RFC3339, expires)
	if err != nil {
		t.Fatalf("expires_at = %q: %v", expires, err)
	}
	// 24h, not the 48h a workspace upload gets.
	if left := time.Until(at); left > anonymousExpiry || left < anonymousExpiry-time.Minute {
		t.Errorf("expires in %v, want about %v", left, anonymousExpiry)
	}
}

func TestClaimedWindowAndNoClaimURLForAKeyedUpload(t *testing.T) {
	c := serve(t, 0)

	done := c.pushAs("krk_live_abc", [2]string{"shot.png", "one"})
	if done["anonymous"] != nil {
		t.Errorf("anonymous = %v, want it absent for a keyed upload", done["anonymous"])
	}
	if done["claim_url"] != nil {
		t.Errorf("claim_url = %v, want none — it already belongs to a workspace", done["claim_url"])
	}

	expires, _ := done["expires_at"].(string)
	at, _ := time.Parse(time.RFC3339, expires)
	if left := time.Until(at); left > workspaceExpiry || left < workspaceExpiry-time.Minute {
		t.Errorf("expires in %v, want about %v", left, workspaceExpiry)
	}
}

// The artifact ID is public — it is in the shareable link. The claim URL is a
// capability, so knowing the ID must not be enough to obtain it.
func TestLookupDoesNotLeakTheClaimURL(t *testing.T) {
	c := serve(t, 0)

	done := c.pushAs("", [2]string{"shot.png", "one"})
	id, _ := done["id"].(string)
	if done["claim_url"] == nil {
		t.Fatal("the push should have returned a claim URL")
	}

	res, err := http.Get(c.url + "/v1/artifacts/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	looked := map[string]any{}
	_ = json.NewDecoder(res.Body).Decode(&looked)

	if res.StatusCode != http.StatusOK {
		t.Fatalf("get = %d", res.StatusCode)
	}
	if looked["claim_url"] != nil {
		t.Errorf("a lookup by ID handed out the claim URL: %v", looked["claim_url"])
	}
	if looked["anonymous"] != true {
		t.Errorf("anonymous = %v, want the status still visible", looked["anonymous"])
	}
}

// Identity is derived from the bytes, so two people holding the same file
// derive the same key. The second one gets the same link — that is the point of
// idempotency — but must not be handed the first one's claim URL, or pushing a
// file someone else had already shared would adopt their upload.
func TestTheClaimURLIsHandedBackOnlyOnce(t *testing.T) {
	c := serve(t, 0)

	first := c.pushAs("", [2]string{"shared.png", "one"})
	if first["claim_url"] == nil {
		t.Fatal("the original push should get a claim URL")
	}

	// Somebody else, same bytes, no key.
	second := c.pushAs("", [2]string{"shared.png", "one"})
	if second["id"] != first["id"] {
		t.Fatalf("identical bytes should resolve to one artifact: %v then %v", first["id"], second["id"])
	}
	if second["claim_url"] != nil {
		t.Errorf("a replay handed out the claim URL again: %v", second["claim_url"])
	}
	// The link itself is still returned, so the retry path stays useful.
	if second["url"] != first["url"] {
		t.Errorf("url = %v, want the original link", second["url"])
	}
}

// Dedup is scoped to an ownership class. A keyed push of bytes somebody had
// already pushed anonymously must mint its own workspace-owned artifact — not
// inherit the anonymous one, with its 24h window and no claim path to recover.
func TestKeyedPushDoesNotInheritAnAnonymousUpload(t *testing.T) {
	c := serve(t, 0)

	anon := c.pushAs("", [2]string{"shared.png", "one"})
	keyed := c.pushAs("krk_live_abc", [2]string{"shared.png", "one"})

	if keyed["id"] == anon["id"] {
		t.Fatalf("the keyed push inherited the anonymous artifact %v", anon["id"])
	}
	if keyed["anonymous"] != nil {
		t.Errorf("anonymous = %v, want the keyed artifact owned by the workspace", keyed["anonymous"])
	}
	if keyed["claim_url"] != nil {
		t.Errorf("claim_url = %v, want none — there is nothing to claim", keyed["claim_url"])
	}
	expires, _ := keyed["expires_at"].(string)
	at, _ := time.Parse(time.RFC3339, expires)
	if left := time.Until(at); left > workspaceExpiry || left < workspaceExpiry-time.Minute {
		t.Errorf("expires in %v, want the workspace window %v", left, workspaceExpiry)
	}

	// And a keyed retry replays the keyed artifact, not the anonymous one.
	again := c.pushAs("krk_live_abc", [2]string{"shared.png", "one"})
	if again["id"] != keyed["id"] {
		t.Errorf("retry replayed %v, want %v", again["id"], keyed["id"])
	}
}

// The finalize retry is partitioned the same way: the right key presented from
// the wrong ownership class does not replay someone else's artifact.
func TestFinalizeRetryIsScopedToTheOwnershipClass(t *testing.T) {
	c := serve(t, 0)
	req := declare([2]string{"shared.png", "one"})

	done := c.pushAs("", [2]string{"shared.png", "one"})
	id, _ := done["id"].(string)

	code, out := c.postAs("/v1/artifacts/"+id+"/finalize", "krk_live_abc",
		map[string]string{"idempotency_key": req.IdempotencyKey})
	if code != http.StatusConflict || out["error"] != "idempotency_key_mismatch" {
		t.Errorf("keyed finalize of an anonymous artifact = %d %v, want 409", code, out)
	}

	// The caller that pushed it still gets the free retry.
	code, out = c.postAs("/v1/artifacts/"+id+"/finalize", "",
		map[string]string{"idempotency_key": req.IdempotencyKey})
	if code != http.StatusOK {
		t.Errorf("anonymous retry = %d %v, want 200", code, out)
	}
}

// finalize stands behind the same gate as begin: an invalid token is a 401,
// not a silent fall into the anonymous class, and a read-only key — which
// shares the writer's workspace class — cannot complete what it could never
// have opened.
func TestFinalizeRejectsInvalidAndReadOnlyKeys(t *testing.T) {
	c := serve(t, 0)
	req := declare([2]string{"shot.png", "one"})

	_, begun := c.beginAs("krk_live_abc", req)
	if code, _ := c.put(begun.Uploads[0].URL, "one"); code != http.StatusOK {
		t.Fatal("put failed")
	}

	finalize := "/v1/artifacts/" + begun.ID + "/finalize"
	body := map[string]string{"idempotency_key": req.IdempotencyKey}

	code, out := c.postAs(finalize, "hunter2", body)
	if code != http.StatusUnauthorized || out["error"] != "invalid_key" {
		t.Errorf("finalize with a garbage token = %d %v, want 401 invalid_key", code, out)
	}

	code, out = c.postAs(finalize, "krk_ro_view", body)
	if code != http.StatusForbidden || out["error"] != "insufficient_scope" {
		t.Errorf("read-only finalize = %d %v, want 403 insufficient_scope", code, out)
	}

	// The writer still completes its own upload.
	if code, done := c.postAs(finalize, "krk_live_abc", body); code != http.StatusOK {
		t.Errorf("the writer's finalize = %d %v, want 200", code, done)
	}
}

// The same 401 protects anonymous pending uploads: `Bearer garbage` is not an
// anonymous caller, so it cannot complete a handshake and take the claim URL.
func TestGarbageTokenCannotFinalizeAnAnonymousUpload(t *testing.T) {
	c := serve(t, 0)
	req := declare([2]string{"shot.png", "one"})

	_, begun := c.begin(req) // anonymous
	if code, _ := c.put(begun.Uploads[0].URL, "one"); code != http.StatusOK {
		t.Fatal("put failed")
	}

	finalize := "/v1/artifacts/" + begun.ID + "/finalize"
	body := map[string]string{"idempotency_key": req.IdempotencyKey}

	code, out := c.postAs(finalize, "hunter2", body)
	if code != http.StatusUnauthorized || out["error"] != "invalid_key" {
		t.Errorf("finalize with a garbage token = %d %v, want 401 invalid_key", code, out)
	}
	if out["claim_url"] != nil {
		t.Errorf("the rejection carried the claim URL: %v", out["claim_url"])
	}

	if code, done := c.postAs(finalize, "", body); code != http.StatusOK || done["claim_url"] == nil {
		t.Errorf("anonymous finalize = %d %v, want 200 with the claim URL", code, done)
	}
}

// Which side of the fence an upload lands on is settled when the handshake
// opens, so it cannot change hands by finalizing with a different key: a keyed
// finalize of an anonymous pending upload is rejected, or the keyed caller
// would walk off with the one-time claim capability.
func TestOwnershipIsDecidedAtTheManifest(t *testing.T) {
	c := serve(t, 0)
	req := declare([2]string{"shot.png", "one"})

	_, begun := c.begin(req) // anonymous
	if code, _ := c.put(begun.Uploads[0].URL, "one"); code != http.StatusOK {
		t.Fatal("put failed")
	}

	code, out := c.postAs("/v1/artifacts/"+begun.ID+"/finalize", "krk_live_abc",
		map[string]string{"idempotency_key": req.IdempotencyKey})
	if code != http.StatusConflict || out["error"] != "idempotency_key_mismatch" {
		t.Fatalf("keyed finalize of an anonymous pending upload = %d %v, want 409", code, out)
	}
	if out["claim_url"] != nil {
		t.Fatalf("the rejection carried the claim URL: %v", out["claim_url"])
	}

	// The anonymous caller that opened the handshake still completes it freely.
	code, done := c.postAs("/v1/artifacts/"+begun.ID+"/finalize", "",
		map[string]string{"idempotency_key": req.IdempotencyKey})
	if code != http.StatusOK {
		t.Fatalf("anonymous finalize = %d %v, want 200", code, done)
	}
	if done["anonymous"] != true {
		t.Errorf("anonymous = %v, want the upload to stay anonymous", done["anonymous"])
	}
	if done["claim_url"] == nil {
		t.Errorf("the completing anonymous caller should receive the claim URL")
	}
}

// Completing the upload earns the claim. Anonymous identity is derived from
// the bytes, so an interrupted anonymous handshake resumes for anyone holding
// the same file — and the claim URL goes to whoever finalizes, exactly once.
// This is the documented trade-off of resumable, content-derived identity.
func TestAnInterruptedAnonymousUploadYieldsItsClaimToTheFinisher(t *testing.T) {
	c := serve(t, 0)
	req := declare([2]string{"shot.png", "one"})

	// The opener pushes every byte but is interrupted before finalizing.
	_, begun := c.begin(req)
	if code, _ := c.put(begun.Uploads[0].URL, "one"); code != http.StatusOK {
		t.Fatal("put failed")
	}

	// A second anonymous pusher of the same bytes resumes the handshake...
	_, resumed := c.begin(req)
	if resumed.ID != begun.ID {
		t.Fatalf("same anonymous bytes should resume one upload: %v then %v", begun.ID, resumed.ID)
	}

	// ...and, by finalizing first, is the one handed the claim URL.
	code, done := c.postAs("/v1/artifacts/"+begun.ID+"/finalize", "",
		map[string]string{"idempotency_key": req.IdempotencyKey})
	if code != http.StatusOK {
		t.Fatalf("finalize = %d %v", code, done)
	}
	if done["claim_url"] == nil {
		t.Fatal("the finalizing caller should receive the claim URL")
	}

	// The opener, finalizing late, gets the artifact back — but the claim
	// capability was minted once, into the response that completed the upload.
	code, late := c.postAs("/v1/artifacts/"+begun.ID+"/finalize", "",
		map[string]string{"idempotency_key": req.IdempotencyKey})
	if code != http.StatusOK {
		t.Fatalf("late finalize = %d %v", code, late)
	}
	if late["claim_url"] != nil {
		t.Errorf("the claim URL was handed out twice: %v", late["claim_url"])
	}
}

// A declared size near int64's ceiling must not wrap the total back under the
// limit, and no single file may exceed the limit — otherwise `put` would hash
// arbitrarily many bytes before its own check could land.
func TestDeclaredSizesCannotOverflowTheTotal(t *testing.T) {
	c := serve(t, 0)

	req := declare([2]string{"a.png", "x"}, [2]string{"b.png", "y"})
	req.Files[0].Bytes = 6 << 60
	req.Files[1].Bytes = 6 << 60 // naive summing wraps negative and slips under

	code, out := c.post("/v1/artifacts", req)
	if code != http.StatusRequestEntityTooLarge || out["error"] != "artifact_too_large" {
		t.Fatalf("overflowing manifest = %d %v, want 413 artifact_too_large", code, out)
	}

	req = declare([2]string{"a.png", "x"})
	req.Files[0].Bytes = DefaultLimitBytes + 1
	code, out = c.post("/v1/artifacts", req)
	if code != http.StatusRequestEntityTooLarge || out["error"] != "artifact_too_large" {
		t.Fatalf("oversized file = %d %v, want 413 artifact_too_large", code, out)
	}
}

// An undecodable finalize body is a broken client, not an absent key — the two
// need different errors or the client cannot tell which it is.
func TestFinalizeRejectsABodyItCannotDecode(t *testing.T) {
	c := serve(t, 0)
	req := declare([2]string{"shot.png", "one"})
	_, begun := c.begin(req)

	res, err := http.Post(c.url+strings.TrimPrefix(begun.FinalizeURL, c.url),
		"application/json", strings.NewReader("{not json"))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(res.Body).Decode(&out)
	if res.StatusCode != http.StatusBadRequest || out["error"] != "malformed_finalize" {
		t.Fatalf("finalize = %d %v, want 400 malformed_finalize", res.StatusCode, out)
	}
}

// Declaring needs no key and sends no bytes, so it is the cheapest request here
// and the one that has to be bounded. Without a cap, every abandoned handshake
// stays in pending, byID and one byToken entry per declared file for the life of
// the process — a loop of declarations grows it until it dies.
func TestAbandonedDeclarationsAreCappedAndExpire(t *testing.T) {
	c, s := serveStore(t, 0)

	for i := range maxPending {
		req := declare([2]string{fmt.Sprintf("shot-%d.png", i), fmt.Sprintf("body %d", i)})
		if code, _ := c.begin(req); code != http.StatusCreated {
			t.Fatalf("declaration %d = %d, want 201", i, code)
		}
	}

	// One past the cap, and nothing has been finalized, so this is refused —
	// retryable, because finishing or abandoning one makes room.
	over := declare([2]string{"one-too-many.png", "body"})
	code, out := c.post("/v1/artifacts", over)
	if code != http.StatusServiceUnavailable || out["error"] != "too_many_pending_uploads" {
		t.Fatalf("over the cap = %d %v, want 503 too_many_pending_uploads", code, out)
	}
	if out["retryable"] != true {
		t.Errorf("retryable = %v, want true", out["retryable"])
	}

	// An hour on, the abandoned ones are swept and the door opens again. The sweep
	// runs on the next declaration rather than on a timer, so this is the path a
	// real client takes.
	s.mu.Lock()
	for _, up := range s.pending {
		up.at = up.at.Add(-2 * pendingTTL)
	}
	s.mu.Unlock()

	if code, _ := c.begin(over); code != http.StatusCreated {
		t.Fatalf("after expiry = %d, want 201", code)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) != 1 {
		t.Errorf("pending = %d, want 1 — the swept declarations should be gone", len(s.pending))
	}
	if len(s.byToken) != 1 {
		t.Errorf("byToken = %d, want 1 — a swept declaration's blob tokens should go with it", len(s.byToken))
	}
	if len(s.byID) != 1 {
		t.Errorf("byID = %d, want 1", len(s.byID))
	}
	if len(s.idOwner) != 1 {
		t.Errorf("idOwner = %d, want 1 — an ID never published goes back in the pool", len(s.idOwner))
	}
}

// A finished upload should not leave its handshake behind either: finalize
// answers a repeat from artifacts, so the byID entry is dead weight after it.
func TestFinalizingReleasesTheHandshake(t *testing.T) {
	c, s := serveStore(t, 0)
	req := declare([2]string{"shot.png", "body"})

	_, begun := c.begin(req)
	if code, _ := c.put(begun.Uploads[0].URL, "body"); code != http.StatusOK {
		t.Fatal("put failed")
	}
	if code, _ := c.postURL(begun.FinalizeURL,
		map[string]string{"idempotency_key": req.IdempotencyKey}); code != http.StatusOK {
		t.Fatal("finalize failed")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) != 0 || len(s.byToken) != 0 || len(s.byID) != 0 {
		t.Errorf("after finalize: pending=%d byToken=%d byID=%d, want all zero",
			len(s.pending), len(s.byToken), len(s.byID))
	}
	// The ID stays spoken for, or a later upload could be handed a link that is
	// already in a pull request.
	if len(s.idOwner) != 1 {
		t.Errorf("idOwner = %d, want the finalized ID still reserved", len(s.idOwner))
	}
}

// One declaration must not be able to mint thousands of blob tokens: the body
// limit alone allows far more entries than any real push has.
func TestAManifestCannotDeclareUnboundedFiles(t *testing.T) {
	c := serve(t, 0)

	files := make([][2]string, 0, maxManifestFiles+1)
	for i := range maxManifestFiles + 1 {
		files = append(files, [2]string{fmt.Sprintf("shot-%d.png", i), fmt.Sprintf("body %d", i)})
	}
	code, out := c.post("/v1/artifacts", declare(files...))
	if code != http.StatusUnprocessableEntity || out["error"] != "too_many_files" {
		t.Fatalf("oversized manifest = %d %v, want 422 too_many_files", code, out)
	}

	// The limit itself is still accepted.
	if code, _ := c.begin(declare(files[:maxManifestFiles]...)); code != http.StatusCreated {
		t.Errorf("a manifest at the limit was refused")
	}
}

func TestGetReturnsFinalizedArtifactsOnly(t *testing.T) {
	c := serve(t, 0)
	req := declare([2]string{"shot.png", "one"})
	_, begun := c.begin(req)

	res, err := http.Get(c.url + "/v1/artifacts/" + begun.ID)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("an unfinalized upload should not resolve, got %d", res.StatusCode)
	}
}
