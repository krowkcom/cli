package registry

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A client names its attempt with an Idempotency-Key, and the same key answers
// with the record it made the first time. What dedupes is the attempt, never the
// content: two declares of identical bytes under different keys are still two
// artifacts with two links.
//
// Pinned here because the stand-in is what the CLI's own suite runs against. A
// stand-in that answered a replay with a second artifact would keep that suite
// green while the real registry behaved differently.

// keyed is request with an Idempotency-Key attached. sent=false leaves the header
// off entirely, which is not the same as sending it empty.
func keyed(t *testing.T, method, url, token, key string, sent bool, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if sent {
		req.Header.Set("Idempotency-Key", key)
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

func declareKeyed(t *testing.T, server *httptest.Server, token, key, filename string) (int, map[string]any) {
	t.Helper()
	return keyed(t, http.MethodPost, server.URL+"/v1/artifacts", token, key, key != "",
		fmt.Sprintf(`{"artifact":{"filename":%q,"content_type":"text/plain","byte_size":9}}`, filename))
}

func openRunKeyed(t *testing.T, server *httptest.Server, token, key, commit string) (int, map[string]any) {
	t.Helper()
	return keyed(t, http.MethodPost, server.URL+"/v1/runs", token, key, key != "",
		fmt.Sprintf(`{"run":{"metadata":{"commit":%q}}}`, commit))
}

func slugOf(t *testing.T, payload map[string]any) string {
	t.Helper()
	slug, _ := payload["slug"].(string)
	if slug == "" {
		t.Fatalf("no slug in %v", payload)
	}
	return slug
}

func TestOneKeyDeclaresOneArtifact(t *testing.T) {
	server, _ := newClockedServer(t)

	_, first := declareKeyed(t, server, "krowk_sk_test", "push-1", "a.txt")
	status, second := declareKeyed(t, server, "krowk_sk_test", "push-1", "a.txt")

	if status != http.StatusCreated {
		t.Fatalf("replay = %d %v, want 201", status, second)
	}
	if slugOf(t, first) != slugOf(t, second) {
		t.Errorf("replay made %s, want the first attempt's %s",
			slugOf(t, second), slugOf(t, first))
	}
}

// A signature lasts 15 minutes while a pending artifact waits far longer, so a
// replay mints a new one rather than repeating a URL that is probably dead.
func TestAReplayIsHandedAFreshUploadURL(t *testing.T) {
	server, clk := newClockedServer(t)

	_, first := declareKeyed(t, server, "krowk_sk_test", "push-1", "a.txt")
	clk.advance(UploadURLLifetime + 1)
	_, second := declareKeyed(t, server, "krowk_sk_test", "push-1", "a.txt")

	if uploadURL(t, first) == uploadURL(t, second) {
		t.Error("replay repeated the first upload url, which has already lapsed")
	}
	if put(t, uploadURL(t, second), "the bytes") != http.StatusOK {
		t.Error("the url a replay handed back does not accept the bytes")
	}
}

func TestAKeyArrivingWithADifferentPayloadIsRefused(t *testing.T) {
	server, _ := newClockedServer(t)

	_, first := declareKeyed(t, server, "krowk_sk_test", "push-1", "a.txt")
	status, payload := declareKeyed(t, server, "krowk_sk_test", "push-1", "b.txt")

	if status != http.StatusConflict {
		t.Fatalf("reuse = %d %v, want 409", status, payload)
	}
	if code := errorCode(payload); code != "idempotency_key_reused" {
		t.Errorf("code = %q, want idempotency_key_reused", code)
	}
	// The message names what the key already made, so a client can tell which
	// record it is being pointed at rather than guessing.
	e, _ := payload["error"].(map[string]any)
	if message, _ := e["message"].(string); !strings.Contains(message, slugOf(t, first)) {
		t.Errorf("message = %q, want the slug the key already created", message)
	}
}

// Canon, glossary.md → Identical bytes do not dedupe. The key is the only thing
// that collapses a create, and without one nothing does.
func TestIdenticalDeclaresWithoutAKeyStayTwoArtifacts(t *testing.T) {
	server, _ := newClockedServer(t)

	first := declare(t, server, "krowk_sk_test", "a.txt", "some bytes")
	second := declare(t, server, "krowk_sk_test", "a.txt", "some bytes")

	if slugOf(t, first) == slugOf(t, second) {
		t.Error("two keyless declares of the same file collapsed into one artifact")
	}
}

func TestADifferentKeyIsADifferentArtifact(t *testing.T) {
	server, _ := newClockedServer(t)

	_, first := declareKeyed(t, server, "krowk_sk_test", "push-1", "a.txt")
	_, second := declareKeyed(t, server, "krowk_sk_test", "push-2", "a.txt")

	if slugOf(t, first) == slugOf(t, second) {
		t.Error("a second key replayed the first key's artifact")
	}
}

// One client's "push-1" must not collide with, or be replayable by, another's.
func TestAKeyIsScopedToTheWorkspaceThatSentIt(t *testing.T) {
	server, _ := newClockedServer(t)

	_, mine := declareKeyed(t, server, "krowk_sk_mine", "push-1", "a.txt")
	status, theirs := declareKeyed(t, server, "krowk_sk_theirs", "push-1", "a.txt")

	if status != http.StatusCreated {
		t.Fatalf("another workspace's declare = %d %v, want 201", status, theirs)
	}
	if slugOf(t, mine) == slugOf(t, theirs) {
		t.Error("one workspace's key answered for another's declare")
	}
}

// A single push opens a run and declares an artifact under one key, so a key
// scoped across both calls would refuse the second half of every push.
func TestOneKeyCoversARunAndAnArtifact(t *testing.T) {
	server, _ := newClockedServer(t)

	if status, payload := openRunKeyed(t, server, "krowk_sk_test", "push-1", "abc"); status != http.StatusCreated {
		t.Fatalf("run = %d %v, want 201", status, payload)
	}
	if status, payload := declareKeyed(t, server, "krowk_sk_test", "push-1", "a.txt"); status != http.StatusCreated {
		t.Fatalf("artifact under the same key = %d %v, want 201", status, payload)
	}
}

// Asked for and not delivered is worse than not asked for: a retry that quietly
// stops deduplicating surfaces on a bill rather than as an error.
func TestAnEmptyKeyIsRefusedRatherThanIgnored(t *testing.T) {
	server, _ := newClockedServer(t)

	status, payload := keyed(t, http.MethodPost, server.URL+"/v1/artifacts", "krowk_sk_test", "", true,
		`{"artifact":{"filename":"a.txt","content_type":"text/plain","byte_size":9}}`)

	if status != http.StatusBadRequest {
		t.Fatalf("empty key = %d %v, want 400", status, payload)
	}
	if code := errorCode(payload); code != "parameter_missing" {
		t.Errorf("code = %q, want parameter_missing", code)
	}
}

// A key outlives its artifact's lifecycle, and takedown keeps the row. Handing a
// replay a fresh PUT over the same storage key would put the bytes a DELETE was
// meant to destroy back where the link resolves.
func TestAReplayWillNotPresignATakenDownArtifact(t *testing.T) {
	server, _ := newClockedServer(t)

	_, first := declareKeyed(t, server, "krowk_sk_test", "push-1", "a.txt")
	slug := slugOf(t, first)

	if status, payload := takeDown(t, server, "krowk_sk_test", slug, ""); status != http.StatusNoContent {
		t.Fatalf("takedown = %d %v, want 204", status, payload)
	}

	status, payload := declareKeyed(t, server, "krowk_sk_test", "push-1", "a.txt")
	if status != http.StatusGone {
		t.Fatalf("replay after takedown = %d %v, want 410", status, payload)
	}
	if code := errorCode(payload); code != "taken_down" {
		t.Errorf("code = %q, want taken_down", code)
	}
}

// Softer than takedown, same hole: a second PUT would overwrite verified bytes
// behind a link people already follow.
func TestAReplayWillNotPresignAFinalizedArtifact(t *testing.T) {
	server, _ := newClockedServer(t)
	const body = "the bytes"

	_, first := declareKeyed(t, server, "krowk_sk_test", "push-1", "a.txt")
	if code := put(t, uploadURL(t, first), body); code != http.StatusOK {
		t.Fatalf("upload = %d", code)
	}
	if status, payload := finalize(t, server, "krowk_sk_test", first); status != http.StatusOK {
		t.Fatalf("finalize = %d %v", status, payload)
	}

	status, payload := declareKeyed(t, server, "krowk_sk_test", "push-1", "a.txt")
	if status != http.StatusConflict {
		t.Fatalf("replay after finalize = %d %v, want 409", status, payload)
	}
	if code := errorCode(payload); code != "already_finalized" {
		t.Errorf("code = %q, want already_finalized", code)
	}
}

func TestAReplayWillNotPresignAnExpiredArtifact(t *testing.T) {
	server, clk := newClockedServer(t)

	declareKeyed(t, server, "", "push-1", "a.txt")
	clk.advance(ephemeralLifetime + 1)

	status, payload := declareKeyed(t, server, "", "push-1", "a.txt")
	if status != http.StatusGone {
		t.Fatalf("replay after expiry = %d %v, want 410", status, payload)
	}
	if code := errorCode(payload); code != "expired" {
		t.Errorf("code = %q, want expired", code)
	}
}

// The registry promises a key is honoured for *at least* a day, and sweeps it
// hourly after that. So a day later it still replays — refusing then would have
// this stand-in turn away retries the real registry answers, and a false refusal
// is the drift that would actually break a client.
func TestAKeyIsStillHonouredADayLater(t *testing.T) {
	server, clk := newClockedServer(t)

	_, first := declareKeyed(t, server, "krowk_sk_test", "push-1", "a.txt")
	clk.advance(25 * time.Hour)

	status, second := declareKeyed(t, server, "krowk_sk_test", "push-1", "a.txt")
	if status != http.StatusCreated {
		t.Fatalf("replay a day later = %d %v, want 201", status, second)
	}
	if slugOf(t, first) != slugOf(t, second) {
		t.Error("a key stopped replaying inside the day it is promised for")
	}
}

func TestOneKeyOpensOneRun(t *testing.T) {
	server, _ := newClockedServer(t)

	_, first := openRunKeyed(t, server, "krowk_sk_test", "push-1", "abc")
	status, second := openRunKeyed(t, server, "krowk_sk_test", "push-1", "abc")

	if status != http.StatusCreated {
		t.Fatalf("replay = %d %v, want 201", status, second)
	}
	if slugOf(t, first) != slugOf(t, second) {
		t.Errorf("replay opened %s, want the first attempt's %s",
			slugOf(t, second), slugOf(t, first))
	}
}

func TestARunKeyArrivingWithDifferentMetadataIsRefused(t *testing.T) {
	server, _ := newClockedServer(t)

	openRunKeyed(t, server, "krowk_sk_test", "push-1", "abc")
	status, payload := openRunKeyed(t, server, "krowk_sk_test", "push-1", "def")

	if status != http.StatusConflict {
		t.Fatalf("reuse = %d %v, want 409", status, payload)
	}
	if code := errorCode(payload); code != "idempotency_key_reused" {
		t.Errorf("code = %q, want idempotency_key_reused", code)
	}
}

// A run has no upload to close and no tombstone, so a replay is the run itself
// however far through its lifecycle it has gone — including finished.
func TestAFinishedRunStillReplays(t *testing.T) {
	server, _ := newClockedServer(t)

	_, first := openRunKeyed(t, server, "krowk_sk_test", "push-1", "abc")
	slug := slugOf(t, first)

	if status, payload := request(t, http.MethodPut,
		server.URL+"/v1/runs/"+slug+"/completion", "krowk_sk_test", "", ""); status != http.StatusOK {
		t.Fatalf("finish = %d %v", status, payload)
	}

	status, second := openRunKeyed(t, server, "krowk_sk_test", "push-1", "abc")
	if status != http.StatusCreated || slugOf(t, second) != slug {
		t.Fatalf("replay = %d %v, want 201 with %s", status, second, slug)
	}
}
