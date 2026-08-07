package registry

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// clock is a movable now, which is the only way a test reaches the expiry
// surface: nothing else can make 24 hours elapse inside one.
type clock struct{ at time.Time }

func (c *clock) now() time.Time          { return c.at }
func (c *clock) advance(d time.Duration) { c.at = c.at.Add(d) }

func newClockedServer(t *testing.T) (*httptest.Server, *clock) {
	t.Helper()
	c := &clock{at: time.Now()}
	server := httptest.NewServer(HandlerWithClock(0, "", c.now))
	t.Cleanup(server.Close)
	return server, c
}

// request is a bare HTTP call: these tests pin the server's contract, so they
// speak wire shapes rather than going through the client that already passes.
func request(t *testing.T, method, url, token, contentType, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var decoded map[string]any
	_ = json.NewDecoder(res.Body).Decode(&decoded)
	return res.StatusCode, decoded
}

func errorCode(payload map[string]any) string {
	e, _ := payload["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

// declare posts an artifact and returns its payload, keyless when token is "".
func declare(t *testing.T, server *httptest.Server, token, filename, body string) map[string]any {
	t.Helper()
	status, payload := request(t, http.MethodPost, server.URL+"/v1/artifacts", token, "application/json",
		fmt.Sprintf(`{"artifact":{"filename":%q,"content_type":"text/plain","byte_size":%d}}`,
			filename, len(body)))
	if status != http.StatusCreated {
		t.Fatalf("declare = %d %v", status, payload)
	}
	return payload
}

func uploadURL(t *testing.T, payload map[string]any) string {
	t.Helper()
	up, _ := payload["upload"].(map[string]any)
	url, _ := up["url"].(string)
	if url == "" {
		t.Fatalf("no upload url in %v", payload)
	}
	return url
}

func put(t *testing.T, url, body string) int {
	t.Helper()
	status, _ := request(t, http.MethodPut, url, "", "text/plain", body)
	return status
}

func finalize(t *testing.T, server *httptest.Server, token string, payload map[string]any) (int, map[string]any) {
	t.Helper()
	slug, _ := payload["slug"].(string)
	return request(t, http.MethodPut, server.URL+"/v1/artifacts/"+slug+"/finalization", token, "", "")
}

// The 24-hour promise and "claim before expiry or lose it" are core behaviour:
// past the lifetime, every endpoint that can meet the artifact answers 410.
func TestEphemeralExpiryAnswersGoneEverywhere(t *testing.T) {
	server, clk := newClockedServer(t)

	payload := declare(t, server, "", "a.txt", "the bytes")
	if put(t, uploadURL(t, payload), "the bytes") != http.StatusOK {
		t.Fatal("upload failed")
	}
	slug, _ := payload["slug"].(string)
	claimToken, _ := payload["claim_token"].(string)

	clk.advance(ephemeralLifetime + time.Minute)

	if status, body := request(t, http.MethodGet, server.URL+"/v1/artifacts/"+slug, "", "", ""); status != http.StatusGone || errorCode(body) != "expired" {
		t.Errorf("show after expiry = %d %v, want 410 expired", status, body)
	}
	if status, body := finalize(t, server, "", payload); status != http.StatusGone || errorCode(body) != "expired" {
		t.Errorf("finalize after expiry = %d %v, want 410 expired", status, body)
	}
	status, body := request(t, http.MethodPost, server.URL+"/v1/artifacts/"+slug+"/claim",
		"krowk_sk_test", "application/json", fmt.Sprintf(`{"claim_token":%q}`, claimToken))
	if status != http.StatusGone || errorCode(body) != "expired" {
		t.Errorf("claim after expiry = %d %v, want 410 expired", status, body)
	}
	// The promise covers the bytes too: the public URL stops serving, as the
	// real registry's lifecycle rule deletes the object.
	url, _ := payload["url"].(string)
	if status, _ := request(t, http.MethodGet, url, "", "", ""); status != http.StatusNotFound {
		t.Errorf("GET %s after expiry = %d, want 404", url, status)
	}
}

func TestClaimJustBeforeExpiryStillSucceeds(t *testing.T) {
	server, clk := newClockedServer(t)

	payload := declare(t, server, "", "a.txt", "the bytes")
	claimToken, _ := payload["claim_token"].(string)
	slug, _ := payload["slug"].(string)

	clk.advance(ephemeralLifetime - time.Minute)

	status, body := request(t, http.MethodPost, server.URL+"/v1/artifacts/"+slug+"/claim",
		"krowk_sk_test", "application/json", fmt.Sprintf(`{"claim_token":%q}`, claimToken))
	if status != http.StatusOK {
		t.Fatalf("claim before expiry = %d %v", status, body)
	}
	// Claimed means kept: the artifact stops expiring.
	clk.advance(48 * time.Hour)
	if status, _ := request(t, http.MethodGet, server.URL+"/v1/artifacts/"+slug, "krowk_sk_test", "", ""); status != http.StatusOK {
		t.Errorf("claimed artifact expired anyway: %d", status)
	}
}

// The upload URL advertises a 15-minute expiry, and real storage enforces the
// signature's window — so the stand-in enforces it too.
func TestLateUploadIsRefused(t *testing.T) {
	server, clk := newClockedServer(t)

	payload := declare(t, server, "", "a.txt", "the bytes")
	clk.advance(uploadURLLifetime + time.Minute)

	if status := put(t, uploadURL(t, payload), "the bytes"); status != http.StatusForbidden {
		t.Errorf("late PUT = %d, want 403", status)
	}
}

// A real presigned URL signs the key: a valid token must not be able to land
// bytes under a different filename — or another workspace's prefix — and then
// finalize an artifact whose canonical link 404s.
func TestUploadTokenIsBoundToTheKey(t *testing.T) {
	server, _ := newClockedServer(t)

	payload := declare(t, server, "", "f.txt", "the bytes")
	rewritten := strings.Replace(uploadURL(t, payload), "/f.txt", "/other.png", 1)
	if status := put(t, rewritten, "the bytes"); status != http.StatusForbidden {
		t.Errorf("PUT to a rewritten filename = %d, want 403", status)
	}
	if status, body := finalize(t, server, "", payload); status != http.StatusConflict {
		t.Errorf("finalize after a refused upload = %d %v, want 409 upload_missing", status, body)
	}

	// The canonical key still works.
	if status := put(t, uploadURL(t, payload), "the bytes"); status != http.StatusOK {
		t.Errorf("canonical PUT = %d, want 200", status)
	}
}

// A claim answers 200 only to the artifact's own token — including on an
// artifact the workspace already holds, where a garbage token must not ride
// the retry-after-success affordance.
func TestClaimRequiresTheRealToken(t *testing.T) {
	server, _ := newClockedServer(t)
	claimPath := func(slug string) string { return server.URL + "/v1/artifacts/" + slug + "/claim" }

	// An artifact pushed with a key was never claimable: any token is a 404.
	owned := declare(t, server, "krowk_sk_test", "a.txt", "the bytes")
	ownedSlug, _ := owned["slug"].(string)
	if status, _ := request(t, http.MethodPost, claimPath(ownedSlug),
		"krowk_sk_test", "application/json", `{"claim_token":"krowk_claim_garbage"}`); status != http.StatusNotFound {
		t.Errorf("claiming a keyed artifact = %d, want 404", status)
	}

	anonymous := declare(t, server, "", "b.txt", "the bytes")
	slug, _ := anonymous["slug"].(string)
	token, _ := anonymous["claim_token"].(string)
	claim := func(key, claimToken string) int {
		status, _ := request(t, http.MethodPost, claimPath(slug),
			key, "application/json", fmt.Sprintf(`{"claim_token":%q}`, claimToken))
		return status
	}

	if got := claim("krowk_sk_test", token); got != http.StatusOK {
		t.Fatalf("claim = %d", got)
	}
	// A retry with the real token is the same success; a garbage token is not.
	if got := claim("krowk_sk_test", token); got != http.StatusOK {
		t.Errorf("retry with the real token = %d, want 200", got)
	}
	if got := claim("krowk_sk_test", "krowk_claim_garbage"); got != http.StatusNotFound {
		t.Errorf("retry with a garbage token = %d, want 404", got)
	}
	// The token was spent for everyone else.
	if got := claim("krowk_sk_other", token); got != http.StatusNotFound {
		t.Errorf("second workspace with the spent token = %d, want 404", got)
	}
}

// Claiming moves the artifact to a new workspace, but the bytes stay where the
// presigned URL was signed for — an upload URL issued before the claim must
// keep working, or a claimed pending artifact could never become ready.
func TestClaimDoesNotInvalidateTheUploadURL(t *testing.T) {
	server, _ := newClockedServer(t)

	payload := declare(t, server, "", "a.txt", "the bytes")
	slug, _ := payload["slug"].(string)
	token, _ := payload["claim_token"].(string)

	status, body := request(t, http.MethodPost, server.URL+"/v1/artifacts/"+slug+"/claim",
		"krowk_sk_test", "application/json", fmt.Sprintf(`{"claim_token":%q}`, token))
	if status != http.StatusOK {
		t.Fatalf("claim = %d %v", status, body)
	}

	if status := put(t, uploadURL(t, payload), "the bytes"); status != http.StatusOK {
		t.Fatalf("PUT after claim = %d, want the pre-claim URL to still work", status)
	}
	if status, body := finalize(t, server, "krowk_sk_test", payload); status != http.StatusOK {
		t.Errorf("finalize after claim = %d %v", status, body)
	}
}

// The stand-in must not be more forgiving than the real thing: a run body that
// does not parse is refused, exactly as a garbage artifact body is.
func TestGarbageRunBodyIsRejected(t *testing.T) {
	server, _ := newClockedServer(t)

	status, body := request(t, http.MethodPost, server.URL+"/v1/runs",
		"krowk_sk_test", "application/json", `{"run": not json`)
	if status != http.StatusBadRequest || errorCode(body) != "parameter_missing" {
		t.Errorf("garbage run body = %d %v, want 400 parameter_missing", status, body)
	}
	// An empty body is still fine: a run needs no metadata.
	if status, _ := request(t, http.MethodPost, server.URL+"/v1/runs", "krowk_sk_test", "", ""); status != http.StatusCreated {
		t.Errorf("empty run body = %d, want 201", status)
	}
}

// Finalizing is documented as idempotent because agents retry: the second
// finalize is the same success carrying the same artifact, not an error and
// not a fresh state transition.
func TestFinalizeTwiceReturnsTheSameArtifact(t *testing.T) {
	server, _ := newClockedServer(t)

	payload := declare(t, server, "krowk_sk_test", "shot.txt", "the bytes")
	if status := put(t, uploadURL(t, payload), "the bytes"); status != http.StatusOK {
		t.Fatalf("put = %d", status)
	}

	status, first := finalize(t, server, "krowk_sk_test", payload)
	if status != http.StatusOK {
		t.Fatalf("first finalize = %d %v", status, first)
	}
	status, second := finalize(t, server, "krowk_sk_test", payload)
	if status != http.StatusOK {
		t.Fatalf("second finalize = %d %v", status, second)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("second finalize answered differently:\nfirst:  %v\nsecond: %v", first, second)
	}
	if first["state"] != "ready" {
		t.Errorf("state = %v, want ready", first["state"])
	}
}

// Completing a run is idempotent too, and the record keeps the moment it first
// finished — a CI cleanup step that runs twice gets the same success both
// times, with the same finished_at.
func TestFinishRunTwiceKeepsTheFirstFinishedAt(t *testing.T) {
	server, clk := newClockedServer(t)

	status, created := request(t, http.MethodPost, server.URL+"/v1/runs",
		"krowk_sk_test", "application/json", `{"run":{}}`)
	if status != http.StatusCreated {
		t.Fatalf("create run = %d %v", status, created)
	}
	slug, _ := created["slug"].(string)

	status, first := request(t, http.MethodPut, server.URL+"/v1/runs/"+slug+"/completion",
		"krowk_sk_test", "", "")
	if status != http.StatusOK || first["status"] != "finished" {
		t.Fatalf("first completion = %d %v", status, first)
	}
	finishedAt, _ := first["finished_at"].(string)
	if finishedAt == "" {
		t.Fatal("no finished_at on the first completion")
	}

	// Enough time passes for a second completion to be visibly a different
	// moment — the answer must keep the first one anyway.
	clk.advance(time.Hour)
	status, second := request(t, http.MethodPut, server.URL+"/v1/runs/"+slug+"/completion",
		"krowk_sk_test", "", "")
	if status != http.StatusOK {
		t.Fatalf("second completion = %d %v", status, second)
	}
	if got, _ := second["finished_at"].(string); got != finishedAt {
		t.Errorf("finished_at moved from %q to %q on a retry", finishedAt, got)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("second completion answered differently:\nfirst:  %v\nsecond: %v", first, second)
	}
}

// openRun opens a run and returns its slug.
func openRun(t *testing.T, server *httptest.Server, token string) string {
	t.Helper()
	status, payload := request(t, http.MethodPost, server.URL+"/v1/runs", token, "application/json", `{"run":{}}`)
	if status != http.StatusCreated {
		t.Fatalf("open run = %d %v", status, payload)
	}
	slug, _ := payload["slug"].(string)
	return slug
}

// Attaching a run after the fact is the route the claim flow needs: a keyless
// upload cannot name a run, and claiming it does not give it one.
func TestAttachRunPutsAnOwnedArtifactUnderARun(t *testing.T) {
	server, _ := newClockedServer(t)
	const token = "krowk_sk_owner"

	artifact := declare(t, server, token, "shot.png", "some bytes")
	slug, _ := artifact["slug"].(string)
	run := openRun(t, server, token)

	url := server.URL + "/v1/artifacts/" + slug + "/run"
	status, payload := request(t, http.MethodPut, url, token, "application/json",
		fmt.Sprintf(`{"run":%q}`, run))
	if status != http.StatusOK || payload["run"] != run {
		t.Fatalf("attach = %d %v, want the artifact under %s", status, payload, run)
	}

	// Idempotent, because agents retry: the same PUT is the same success.
	if again, body := request(t, http.MethodPut, url, token, "application/json",
		fmt.Sprintf(`{"run":%q}`, run)); again != http.StatusOK || body["run"] != run {
		t.Errorf("second attach = %d %v", again, body)
	}

	// PATCH is routed alongside PUT, as the registry's own routes are.
	if status, body := request(t, http.MethodPatch, url, token, "application/json",
		fmt.Sprintf(`{"run":%q}`, run)); status != http.StatusOK || body["run"] != run {
		t.Errorf("PATCH alias = %d %v", status, body)
	}

	// The link is what has already been pasted, so attaching must not move it.
	if shown := mustShow(t, server, token, slug); shown["url"] != artifact["url"] {
		t.Errorf("attaching moved the link: %v → %v", artifact["url"], shown["url"])
	}

	// A closed run still accepts one. The case this exists for is a CI run that
	// finished long before anyone claimed the anonymous upload it left behind, so
	// refusing here would leave that upload nowhere to go.
	if status, payload := request(t, http.MethodPut,
		server.URL+"/v1/runs/"+run+"/completion", token, "", ""); status != http.StatusOK {
		t.Fatalf("finish run = %d %v", status, payload)
	}
	second := declare(t, server, token, "later.png", "some bytes")
	laterSlug, _ := second["slug"].(string)
	if status, payload := request(t, http.MethodPut,
		server.URL+"/v1/artifacts/"+laterSlug+"/run", token, "application/json",
		fmt.Sprintf(`{"run":%q}`, run)); status != http.StatusOK || payload["run"] != run {
		t.Errorf("attach to a finished run = %d %v, want it allowed", status, payload)
	}
}

func mustShow(t *testing.T, server *httptest.Server, token, slug string) map[string]any {
	t.Helper()
	status, payload := request(t, http.MethodGet, server.URL+"/v1/artifacts/"+slug, token, "", "")
	if status != http.StatusOK {
		t.Fatalf("show = %d %v", status, payload)
	}
	return payload
}

// A run belongs to a workspace, so a keyless attach is refused with the same code
// a keyless push naming a run gets — and the run parameter is required.
func TestAttachRunNeedsAKeyAndARun(t *testing.T) {
	server, _ := newClockedServer(t)
	const token = "krowk_sk_owner"

	artifact := declare(t, server, token, "shot.png", "some bytes")
	slug, _ := artifact["slug"].(string)
	run := openRun(t, server, token)
	url := server.URL + "/v1/artifacts/" + slug + "/run"

	// A plain 401, the way the registry refuses this route — not the run_needs_key a
	// keyless push naming a run gets, which is raised only where a request may
	// legitimately arrive without a key.
	status, payload := request(t, http.MethodPut, url, "", "application/json", fmt.Sprintf(`{"run":%q}`, run))
	if status != http.StatusUnauthorized || errorCode(payload) != "unauthorized" {
		t.Errorf("keyless attach = %d %s, want 401 unauthorized", status, errorCode(payload))
	}

	status, payload = request(t, http.MethodPut, url, token, "application/json", `{}`)
	if status != http.StatusBadRequest || errorCode(payload) != "parameter_missing" {
		t.Errorf("attach with no run = %d %s, want 400 parameter_missing", status, errorCode(payload))
	}
}

// Both slugs resolve in the requesting key's workspace, so another workspace's
// artifact and another workspace's run are both simply not there. That is also
// what makes claim-then-attach the required order: an unclaimed anonymous upload
// is in nobody's workspace.
func TestAttachRunIsScopedToTheKeysWorkspace(t *testing.T) {
	server, _ := newClockedServer(t)
	const owner, stranger = "krowk_sk_owner", "krowk_sk_stranger"

	artifact := declare(t, server, owner, "shot.png", "some bytes")
	slug, _ := artifact["slug"].(string)
	run := openRun(t, server, owner)
	url := server.URL + "/v1/artifacts/" + slug + "/run"

	if status, payload := request(t, http.MethodPut, url, stranger, "application/json",
		fmt.Sprintf(`{"run":%q}`, run)); status != http.StatusNotFound || errorCode(payload) != "not_found" {
		t.Errorf("a stranger attached it: %d %s", status, errorCode(payload))
	}
	if status, payload := request(t, http.MethodPut, url, owner, "application/json",
		`{"run":"run_nosuchrunatall00000"}`); status != http.StatusNotFound || errorCode(payload) != "not_found" {
		t.Errorf("unknown run = %d %s, want 404 not_found", status, errorCode(payload))
	}
	// Someone else's run is the same answer, not a hint that it exists.
	strangersRun := openRun(t, server, stranger)
	if status, payload := request(t, http.MethodPut, url, owner, "application/json",
		fmt.Sprintf(`{"run":%q}`, strangersRun)); status != http.StatusNotFound {
		t.Errorf("attached to another workspace's run: %d %v", status, payload)
	}

	anonymous := declare(t, server, "", "anon.png", "some bytes")
	anonSlug, _ := anonymous["slug"].(string)
	if status, payload := request(t, http.MethodPut,
		server.URL+"/v1/artifacts/"+anonSlug+"/run", owner, "application/json",
		fmt.Sprintf(`{"run":%q}`, run)); status != http.StatusNotFound {
		t.Errorf("an unclaimed anonymous upload was attachable: %d %v", status, payload)
	}
}

// The real presign signs the digest as a header, so storage refuses the PUT
// outright when it is missing — the signature does not verify without it. The
// header therefore has to be handed over and has to be required, or a client
// that ignores upload.headers passes here and fails against R2.
func TestUploadRequiresTheChecksumHeaderItWasHanded(t *testing.T) {
	server, _ := newClockedServer(t)
	body := "the bytes"
	sum := sha256.Sum256([]byte(body))
	encoded := base64.StdEncoding.EncodeToString(sum[:])

	status, payload := request(t, http.MethodPost, server.URL+"/v1/artifacts", "", "application/json",
		fmt.Sprintf(`{"artifact":{"filename":"a.txt","content_type":"text/plain","byte_size":%d,"checksum":%q}}`,
			len(body), hex.EncodeToString(sum[:])))
	if status != http.StatusCreated {
		t.Fatalf("declare = %d %v", status, payload)
	}

	up, _ := payload["upload"].(map[string]any)
	headers, _ := up["headers"].(map[string]any)
	if got, _ := headers["x-amz-checksum-sha256"].(string); got != encoded {
		t.Errorf("upload.headers[x-amz-checksum-sha256] = %q, want %q", got, encoded)
	}

	url := uploadURL(t, payload)
	if got := put(t, url, body); got != http.StatusForbidden {
		t.Errorf("PUT without the checksum header = %d, want 403", got)
	}

	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("x-amz-checksum-sha256", encoded)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("PUT with the checksum header = %d, want 200", res.StatusCode)
	}
}

// takeDown issues the takedown, with a claim token when one is given.
func takeDown(t *testing.T, server *httptest.Server, token, slug, claimToken string) (int, map[string]any) {
	t.Helper()
	body, contentType := "", ""
	if claimToken != "" {
		body = fmt.Sprintf(`{"claim_token":%q}`, claimToken)
		contentType = "application/json"
	}
	return request(t, http.MethodDelete, server.URL+"/v1/artifacts/"+slug, token, contentType, body)
}

// readyArtifact declares, uploads and finalizes, so a test starts from the state
// a takedown actually meets in practice.
func readyArtifact(t *testing.T, server *httptest.Server, token, body string) map[string]any {
	t.Helper()
	payload := declare(t, server, token, "a.txt", body)
	if put(t, uploadURL(t, payload), body) != http.StatusOK {
		t.Fatal("upload failed")
	}
	if status, final := finalize(t, server, token, payload); status != http.StatusOK {
		t.Fatalf("finalize = %d %v", status, final)
	}
	return payload
}

// Takedown is the path someone reaches for when a secret was published by
// accident, so the bytes have to be gone at once — and what stays behind is a
// tombstone, because the link is already pasted somewhere and its reader
// deserves 410 rather than being sent hunting for a typo.
func TestTakedownRemovesTheBytesAndAnswersGoneEverywhere(t *testing.T) {
	server, _ := newClockedServer(t)
	const key = "krowk_sk_test"

	payload := readyArtifact(t, server, key, "a secret")
	slug, _ := payload["slug"].(string)
	url, _ := payload["url"].(string)

	if status, body := takeDown(t, server, key, slug, ""); status != http.StatusNoContent {
		t.Fatalf("takedown = %d %v, want 204", status, body)
	}

	// The bytes go first, and they go for good: a takedown that could be undone
	// would leave a leaked secret leaked.
	if status, _ := request(t, http.MethodGet, url, "", "", ""); status != http.StatusNotFound {
		t.Errorf("GET %s after takedown = %d, want 404", url, status)
	}
	if status, body := request(t, http.MethodGet, server.URL+"/v1/artifacts/"+slug, key, "", ""); status != http.StatusGone || errorCode(body) != "taken_down" {
		t.Errorf("show after takedown = %d %v, want 410 taken_down", status, body)
	}
	// Finalizing has to report the takedown rather than its own idempotent
	// success: a taken-down artifact is normally a ready one, and answering 200
	// would hand back a url pointing at bytes that are no longer there.
	if status, body := finalize(t, server, key, payload); status != http.StatusGone || errorCode(body) != "taken_down" {
		t.Errorf("finalize after takedown = %d %v, want 410 taken_down", status, body)
	}

	status, created := request(t, http.MethodPost, server.URL+"/v1/runs", key, "application/json", `{"run":{}}`)
	if status != http.StatusCreated {
		t.Fatalf("create run = %d %v", status, created)
	}
	runSlug, _ := created["slug"].(string)
	status, body := request(t, http.MethodPut, server.URL+"/v1/artifacts/"+slug+"/run",
		key, "application/json", fmt.Sprintf(`{"run":%q}`, runSlug))
	if status != http.StatusGone || errorCode(body) != "taken_down" {
		t.Errorf("attach after takedown = %d %v, want 410 taken_down", status, body)
	}

	// A tombstone is not something the workspace still holds, so it answers for
	// its own slug and nowhere else.
	status, listing := request(t, http.MethodGet, server.URL+"/v1/artifacts", key, "", "")
	if status != http.StatusOK {
		t.Fatalf("list = %d %v", status, listing)
	}
	if artifacts, _ := listing["artifacts"].([]any); len(artifacts) != 0 {
		t.Errorf("listing still holds the tombstone: %v", artifacts)
	}

	// Idempotent, like finalizing: taking down what is already down is a success,
	// so a retry after a lost response is not a 404.
	if status, body := takeDown(t, server, key, slug, ""); status != http.StatusNoContent {
		t.Errorf("second takedown = %d %v, want 204", status, body)
	}
}

// For a keyless caller the claim token is the authority and the slug is not.
// Reading by slug alone is right — the bytes are public on the CDN regardless —
// but a slug travels in whatever the link was pasted into, so a takedown
// authorised by slug would let any reader of the paste destroy what they read.
func TestKeylessTakedownNeedsTheClaimTokenAndNotJustTheSlug(t *testing.T) {
	server, _ := newClockedServer(t)

	payload := readyArtifact(t, server, "", "the bytes")
	slug, _ := payload["slug"].(string)
	claimToken, _ := payload["claim_token"].(string)

	if status, body := takeDown(t, server, "", slug, ""); status != http.StatusBadRequest || errorCode(body) != "parameter_missing" {
		t.Errorf("takedown with no token = %d %v, want 400 parameter_missing", status, body)
	}
	if status, body := takeDown(t, server, "", slug, "krowk_claim_wrong"); status != http.StatusNotFound || errorCode(body) != "not_found" {
		t.Errorf("takedown with a wrong token = %d %v, want 404", status, body)
	}
	if status, _ := request(t, http.MethodGet, server.URL+"/v1/artifacts/"+slug, "", "", ""); status != http.StatusOK {
		t.Error("a refused takedown took the artifact down anyway")
	}

	if status, body := takeDown(t, server, "", slug, claimToken); status != http.StatusNoContent {
		t.Fatalf("takedown with the real token = %d %v, want 204", status, body)
	}
	if status, body := request(t, http.MethodGet, server.URL+"/v1/artifacts/"+slug, "", "", ""); status != http.StatusGone || errorCode(body) != "taken_down" {
		t.Errorf("show after takedown = %d %v, want 410 taken_down", status, body)
	}
}

// A key's authority is the workspace it acts in, so another tenant's slug reads
// as simply not existing rather than as forbidden — which would confirm it does.
func TestTakedownIsScopedToTheKeysWorkspace(t *testing.T) {
	server, _ := newClockedServer(t)

	payload := readyArtifact(t, server, "krowk_sk_mine", "the bytes")
	slug, _ := payload["slug"].(string)

	if status, body := takeDown(t, server, "krowk_sk_theirs", slug, ""); status != http.StatusNotFound {
		t.Errorf("takedown across workspaces = %d %v, want 404", status, body)
	}
	if status, _ := request(t, http.MethodGet, server.URL+"/v1/artifacts/"+slug, "krowk_sk_mine", "", ""); status != http.StatusOK {
		t.Error("another workspace's takedown went through")
	}
}

// Claiming spends the token and moves the artifact into a real workspace, so
// from then on the key that claimed it is what takes it down. A token that no
// longer speaks for anything must not keep speaking for this.
func TestSpentClaimTokenNoLongerTakesAnArtifactDown(t *testing.T) {
	server, _ := newClockedServer(t)
	const key = "krowk_sk_test"

	payload := readyArtifact(t, server, "", "the bytes")
	slug, _ := payload["slug"].(string)
	claimToken, _ := payload["claim_token"].(string)

	status, claimed := request(t, http.MethodPost, server.URL+"/v1/artifacts/"+slug+"/claim",
		key, "application/json", fmt.Sprintf(`{"claim_token":%q}`, claimToken))
	if status != http.StatusOK {
		t.Fatalf("claim = %d %v", status, claimed)
	}

	if status, body := takeDown(t, server, "", slug, claimToken); status != http.StatusNotFound {
		t.Errorf("takedown with a spent token = %d %v, want 404", status, body)
	}
	if status, body := takeDown(t, server, key, slug, ""); status != http.StatusNoContent {
		t.Errorf("takedown by the claiming key = %d %v, want 204", status, body)
	}
}

// Nothing claims an artifact back out of a takedown, and the registry says so as
// a 404 rather than a 410: it reaches the artifact through Artifact.claimable,
// which narrows to `live`, so a tombstone is not found rather than gone. Getting
// this wrong would have a client branch differently against --dev than against
// production, which is the whole failure a stand-in exists to prevent.
func TestATakenDownArtifactCannotBeClaimedBack(t *testing.T) {
	server, _ := newClockedServer(t)

	payload := readyArtifact(t, server, "", "the bytes")
	slug, _ := payload["slug"].(string)
	claimToken, _ := payload["claim_token"].(string)

	if status, body := takeDown(t, server, "", slug, claimToken); status != http.StatusNoContent {
		t.Fatalf("takedown = %d %v", status, body)
	}

	status, body := request(t, http.MethodPost, server.URL+"/v1/artifacts/"+slug+"/claim",
		"krowk_sk_test", "application/json", fmt.Sprintf(`{"claim_token":%q}`, claimToken))
	if status != http.StatusNotFound || errorCode(body) != "not_found" {
		t.Errorf("claim after takedown = %d %v, want 404 not_found", status, body)
	}
}

// An artifact can be both taken down and past its expiry, and the one somebody
// decided is the truer answer — so the order the two are reported in is load
// bearing rather than incidental.
func TestTakedownIsReportedAheadOfAnExpiry(t *testing.T) {
	server, clk := newClockedServer(t)

	payload := readyArtifact(t, server, "", "the bytes")
	slug, _ := payload["slug"].(string)
	claimToken, _ := payload["claim_token"].(string)

	if status, _ := takeDown(t, server, "", slug, claimToken); status != http.StatusNoContent {
		t.Fatal("takedown failed")
	}
	clk.advance(ephemeralLifetime + time.Minute)

	if status, body := request(t, http.MethodGet, server.URL+"/v1/artifacts/"+slug, "", "", ""); status != http.StatusGone || errorCode(body) != "taken_down" {
		t.Errorf("show = %d %v, want 410 taken_down rather than expired", status, body)
	}
}

// A takedown spends whatever upload URL is still outstanding. Without that, an
// artifact taken down before its bytes arrived would accept them afterwards and
// resurrect the public link under a tombstone.
func TestTakedownSpendsAnOutstandingUploadURL(t *testing.T) {
	server, _ := newClockedServer(t)
	const body = "the bytes"

	// Declared but deliberately not uploaded, so the presigned PUT is still live.
	payload := declare(t, server, "", "a.txt", body)
	slug, _ := payload["slug"].(string)
	claimToken, _ := payload["claim_token"].(string)
	url := uploadURL(t, payload)

	if status, got := takeDown(t, server, "", slug, claimToken); status != http.StatusNoContent {
		t.Fatalf("takedown = %d %v", status, got)
	}

	if status := put(t, url, body); status != http.StatusForbidden {
		t.Errorf("PUT after takedown = %d, want 403 — the URL is spent", status)
	}
	public, _ := payload["url"].(string)
	if status, _ := request(t, http.MethodGet, public, "", "", ""); status != http.StatusNotFound {
		t.Errorf("GET %s after takedown = %d, want 404", public, status)
	}
}

func slugsOf(t *testing.T, payload map[string]any, key string) []string {
	t.Helper()
	rows, _ := payload[key].([]any)
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		record, _ := row.(map[string]any)
		slug, _ := record["slug"].(string)
		out = append(out, slug)
	}
	return out
}

// Runs page the way artifacts do — newest first, "older than this one" rather
// than an offset, so rows are neither skipped nor repeated when one is opened
// mid-listing.
func TestRunListingPagesNewestFirst(t *testing.T) {
	server, _ := newClockedServer(t)
	const key = "krowk_sk_test"

	var opened []string
	for range 3 {
		opened = append(opened, openRun(t, server, key))
	}
	newest, middle, oldest := opened[2], opened[1], opened[0]

	status, first := request(t, http.MethodGet, server.URL+"/v1/runs?limit=2", key, "", "")
	if status != http.StatusOK {
		t.Fatalf("list runs = %d %v", status, first)
	}
	if got := slugsOf(t, first, "runs"); !reflect.DeepEqual(got, []string{newest, middle}) {
		t.Errorf("first page = %v, want the two newest in order", got)
	}
	// next is present because the page came back full, and carries the cursor.
	if got, _ := first["next"].(string); got != middle {
		t.Errorf("next = %q, want %q", got, middle)
	}

	_, second := request(t, http.MethodGet, server.URL+"/v1/runs?limit=2&before="+middle, key, "", "")
	if got := slugsOf(t, second, "runs"); !reflect.DeepEqual(got, []string{oldest}) {
		t.Errorf("second page = %v, want the oldest alone", got)
	}
	if second["next"] != nil {
		t.Errorf("next = %v, want null on the last page", second["next"])
	}

	// Another key sees none of them.
	_, theirs := request(t, http.MethodGet, server.URL+"/v1/runs", "krowk_sk_theirs", "", "")
	if got := slugsOf(t, theirs, "runs"); len(got) != 0 {
		t.Errorf("another workspace's listing = %v, want empty", got)
	}
}

func TestShowRunIsScopedToTheKeysWorkspace(t *testing.T) {
	server, _ := newClockedServer(t)

	status, opened := request(t, http.MethodPost, server.URL+"/v1/runs", "krowk_sk_mine",
		"application/json", `{"run":{"metadata":{"repo":"acme/storefront"}}}`)
	if status != http.StatusCreated {
		t.Fatalf("open run = %d %v", status, opened)
	}
	slug, _ := opened["slug"].(string)

	status, payload := request(t, http.MethodGet, server.URL+"/v1/runs/"+slug, "krowk_sk_mine", "", "")
	if status != http.StatusOK {
		t.Fatalf("show run = %d %v", status, payload)
	}
	metadata, _ := payload["metadata"].(map[string]any)
	if metadata["repo"] != "acme/storefront" {
		t.Errorf("metadata = %v, want the run's own", payload["metadata"])
	}

	if status, _ := request(t, http.MethodGet, server.URL+"/v1/runs/"+slug, "krowk_sk_theirs", "", ""); status != http.StatusNotFound {
		t.Errorf("show across workspaces = %d, want 404", status)
	}
	if status, _ := request(t, http.MethodGet, server.URL+"/v1/runs/"+slug, "", "", ""); status != http.StatusUnauthorized {
		t.Errorf("show without a key = %d, want 401", status)
	}
}

// A run's artifacts are a collection of the run, not a filter on the workspace
// listing. The difference is the whole reason for the endpoint: an unknown run
// is a 404 from the run itself, where a filter would answer an empty page — and
// a caller cannot tell that apart from a run that produced nothing.
func TestRunArtifactsAreACollectionOfTheRunRatherThanAFilter(t *testing.T) {
	server, _ := newClockedServer(t)
	const key = "krowk_sk_test"

	mine := openRun(t, server, key)
	other := openRun(t, server, key)

	inRun := declare(t, server, key, "a.txt", "aa")
	slug, _ := inRun["slug"].(string)
	status, body := request(t, http.MethodPut, server.URL+"/v1/artifacts/"+slug+"/run",
		key, "application/json", fmt.Sprintf(`{"run":%q}`, mine))
	if status != http.StatusOK {
		t.Fatalf("attach = %d %v", status, body)
	}
	declare(t, server, key, "loose.txt", "bb") // in the workspace, in no run

	status, page := request(t, http.MethodGet, server.URL+"/v1/runs/"+mine+"/artifacts", key, "", "")
	if status != http.StatusOK {
		t.Fatalf("run artifacts = %d %v", status, page)
	}
	if got := slugsOf(t, page, "artifacts"); !reflect.DeepEqual(got, []string{slug}) {
		t.Errorf("run artifacts = %v, want only what the run made", got)
	}

	// A run that made nothing is an empty page, and it is answerable.
	_, empty := request(t, http.MethodGet, server.URL+"/v1/runs/"+other+"/artifacts", key, "", "")
	if got := slugsOf(t, empty, "artifacts"); len(got) != 0 {
		t.Errorf("empty run = %v, want no artifacts", got)
	}
	// A run that does not exist is not the same thing, and must not read as one.
	if status, _ := request(t, http.MethodGet, server.URL+"/v1/runs/run_nosuchrun0000000000/artifacts", key, "", ""); status != http.StatusNotFound {
		t.Errorf("unknown run = %d, want 404 rather than an empty page", status)
	}
	// Nor is another workspace's run, which reads as not existing rather than as
	// forbidden — which would confirm that it does.
	if status, _ := request(t, http.MethodGet, server.URL+"/v1/runs/"+mine+"/artifacts", "krowk_sk_theirs", "", ""); status != http.StatusNotFound {
		t.Errorf("another workspace's run = %d, want 404", status)
	}
}

// The `next` rule is the one part of the pagination contract a client cannot
// discover for itself, and it is deliberately not "next when rows were
// truncated": it is present whenever the page came back full, so a total that is
// an exact multiple of the limit costs one extra empty page rather than a count
// query on every listing. Rows == limit exactly is the only case that tells the
// two rules apart.
func TestAFullPageAlwaysCarriesACursorEvenWhenItIsTheLast(t *testing.T) {
	server, _ := newClockedServer(t)
	const key = "krowk_sk_test"

	first := openRun(t, server, key)
	second := openRun(t, server, key)

	status, page := request(t, http.MethodGet, server.URL+"/v1/runs?limit=2", key, "", "")
	if status != http.StatusOK {
		t.Fatalf("list = %d %v", status, page)
	}
	if got := slugsOf(t, page, "runs"); len(got) != 2 {
		t.Fatalf("page = %v, want both runs", got)
	}
	// Exactly as many rows as asked for, and nothing behind them — the cursor is
	// still handed over rather than withheld.
	next, _ := page["next"].(string)
	if next != first {
		t.Fatalf("next = %v, want %q even though this is the last page", page["next"], first)
	}

	_, beyond := request(t, http.MethodGet, server.URL+"/v1/runs?limit=2&before="+next, key, "", "")
	if got := slugsOf(t, beyond, "runs"); len(got) != 0 {
		t.Errorf("page after the cursor = %v, want it empty", got)
	}
	if beyond["next"] != nil {
		t.Errorf("next = %v, want null once the rows run out", beyond["next"])
	}
	_ = second
}

// The page size is the caller's to ask for and the registry's to bound. Asking
// for more than is served gets the most that is served — which is what was
// wanted — and anything that is not a number is not a request for one.
func TestPageSizeIsClampedRatherThanRefused(t *testing.T) {
	server, _ := newClockedServer(t)
	const key = "krowk_sk_test"

	for range maxPageSize + 2 {
		openRun(t, server, key)
	}

	for _, limit := range []string{"", "0", "-1", "abc", "999999999999999999999"} {
		url := server.URL + "/v1/runs"
		if limit != "" {
			url += "?limit=" + limit
		}
		status, page := request(t, http.MethodGet, url, key, "", "")
		if status != http.StatusOK {
			t.Errorf("limit=%q = %d %v, want 200", limit, status, page)
			continue
		}
		got := len(slugsOf(t, page, "runs"))
		// An unreadable or out-of-range limit is the default; a readable one is
		// clamped into the served range. Neither is ever unbounded.
		want := defaultPageSize
		if limit == "0" || limit == "-1" {
			want = 1
		}
		if got != want {
			t.Errorf("limit=%q served %d rows, want %d", limit, got, want)
		}
	}

	_, page := request(t, http.MethodGet, server.URL+"/v1/runs?limit=999", key, "", "")
	if got := len(slugsOf(t, page, "runs")); got != maxPageSize {
		t.Errorf("limit=999 served %d rows, want the %d ceiling", got, maxPageSize)
	}
}

// A run's artifact listing is scoped three ways, and each one is a separate way
// to serve the wrong rows: the cursor has to belong to this run, an unknown
// cursor is a 404 rather than a silent first page, and a tombstone is not
// something the run still holds.
func TestARunsArtifactListingIsScopedToTheRunAndToWhatItStillHolds(t *testing.T) {
	server, _ := newClockedServer(t)
	const key = "krowk_sk_test"

	mine := openRun(t, server, key)
	other := openRun(t, server, key)

	attach := func(runSlug, name string) string {
		payload := readyArtifact(t, server, key, "bytes for "+name)
		slug, _ := payload["slug"].(string)
		status, body := request(t, http.MethodPut, server.URL+"/v1/artifacts/"+slug+"/run",
			key, "application/json", fmt.Sprintf(`{"run":%q}`, runSlug))
		if status != http.StatusOK {
			t.Fatalf("attach %s = %d %v", name, status, body)
		}
		return slug
	}
	older := attach(mine, "older")
	newer := attach(mine, "newer")
	elsewhere := attach(other, "elsewhere")

	listing := func(query string) (int, map[string]any) {
		return request(t, http.MethodGet, server.URL+"/v1/runs/"+mine+"/artifacts"+query, key, "", "")
	}

	// A cursor from another run is not a cursor for this one.
	if status, _ := listing("?before=" + elsewhere); status != http.StatusNotFound {
		t.Errorf("cursor from another run = %d, want 404", status)
	}
	// Nor is one that names nothing at all — answering the first page instead
	// would silently ignore the caller's position.
	if status, _ := listing("?before=art_nosuchartifact00001"); status != http.StatusNotFound {
		t.Errorf("unknown cursor = %d, want 404", status)
	}
	// A cursor of this run's own pages within it.
	_, page := listing("?before=" + newer)
	if got := slugsOf(t, page, "artifacts"); !reflect.DeepEqual(got, []string{older}) {
		t.Errorf("page after the newest = %v, want the older one alone", got)
	}

	// A tombstone is not part of what the run still holds, exactly as it is not
	// part of what the workspace holds.
	if status, body := takeDown(t, server, key, older, ""); status != http.StatusNoContent {
		t.Fatalf("takedown = %d %v", status, body)
	}
	_, remaining := listing("")
	if got := slugsOf(t, remaining, "artifacts"); !reflect.DeepEqual(got, []string{newer}) {
		t.Errorf("run listing after a takedown = %v, want the tombstone gone", got)
	}
}
