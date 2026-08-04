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
	srv := httptest.NewServer(Handler(limit, ""))
	t.Cleanup(srv.Close)
	return &client{t: t, url: srv.URL}
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
