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
