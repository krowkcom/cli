package registry

import (
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
