package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testClient wires a Client to a server with the Sleep seam recording rather
// than sleeping, which is what the seam exists for.
func testClient(server *httptest.Server) (*Client, *[]time.Duration) {
	var slept []time.Duration
	c := New(server.URL+"/v1", "krowk_sk_test")
	c.Sleep = func(d time.Duration) { slept = append(slept, d) }
	return c, &slept
}

// A retryable failure followed by a success must end in the success — and the
// wait between them is the server's Retry-After, not the default backoff.
func TestRetryableFailureThenSuccessHonoursRetryAfter(t *testing.T) {
	var mu sync.Mutex
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":{"code":"overloaded","message":"try later"}}`)
			return
		}
		fmt.Fprint(w, `{"slug":"art_x","state":"ready"}`)
	}))
	t.Cleanup(server.Close)

	c, slept := testClient(server)
	artifact, err := c.ShowArtifact(context.Background(), "art_x")
	if err != nil {
		t.Fatalf("ShowArtifact = %v, want the retry to succeed", err)
	}
	if artifact.Slug != "art_x" {
		t.Errorf("slug = %q", artifact.Slug)
	}
	if len(*slept) != 1 || (*slept)[0] != 7*time.Second {
		t.Errorf("slept %v, want exactly the server's Retry-After of 7s", *slept)
	}
}

// A server that never recovers gets exactly maxAttempts requests — an
// off-by-one in the loop bound would give up early or storm one extra, and
// nothing else in the suite would notice.
func TestRetriesStopAtTheAttemptCap(t *testing.T) {
	var mu sync.Mutex
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"code":"boom","message":"still down"}}`)
	}))
	t.Cleanup(server.Close)

	c, slept := testClient(server)
	_, err := c.ShowArtifact(context.Background(), "art_x")
	if err == nil {
		t.Fatal("want the exhausted failure back")
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code() != "boom" {
		t.Errorf("err = %v, want the last attempt's error", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != maxAttempts {
		t.Errorf("calls = %d, want exactly maxAttempts (%d)", calls, maxAttempts)
	}
	if len(*slept) != maxAttempts-1 {
		t.Errorf("slept %v, want a backoff between each attempt and none after the last", *slept)
	}
}

// A failure the client must not retry gets no second request and no sleep.
func TestNonRetryableFailureIsNotRetried(t *testing.T) {
	var mu sync.Mutex
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusUnprocessableEntity)
		fmt.Fprint(w, `{"error":{"code":"invalid","message":"no"}}`)
	}))
	t.Cleanup(server.Close)

	c, slept := testClient(server)
	if _, err := c.ShowArtifact(context.Background(), "art_x"); err == nil {
		t.Fatal("want the 422 back")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 || len(*slept) != 0 {
		t.Errorf("calls = %d, slept = %v — a 422 must not be retried", calls, *slept)
	}
}

// The PUT retry reopens the file per attempt, so the second attempt must carry
// the complete body — a half-consumed reader would send nothing.
func TestPutBytesRetrySendsTheFullBodyAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shot.png")
	contents := "the whole file, every attempt"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		n := len(bodies)
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	c, slept := testClient(server)
	up := &Upload{
		Method:  http.MethodPut,
		URL:     server.URL + "/_storage/ws/art/shot.png",
		Headers: map[string]string{"Content-Type": "image/png"},
	}
	spec := Spec{Path: path, ByteSize: int64(len(contents))}

	if err := c.PutBytes(context.Background(), up, spec); err != nil {
		t.Fatalf("PutBytes = %v, want the retry to succeed", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("attempts = %d, want the 500 retried once", len(bodies))
	}
	for i, b := range bodies {
		if b != contents {
			t.Errorf("attempt %d sent %q, want the full file", i+1, b)
		}
	}
	if len(*slept) != 1 || (*slept)[0] != 500*time.Millisecond {
		t.Errorf("slept %v, want the first attempt's default backoff of 500ms", *slept)
	}
}

// PutBytes has its own retry loop with the same cap, so it gets the same
// exhaustion pin: a storage host that never recovers sees exactly maxAttempts
// uploads.
func TestPutBytesStopsAtTheAttemptCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(path, []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	c, slept := testClient(server)
	up := &Upload{Method: http.MethodPut, URL: server.URL + "/_storage/ws/art/shot.png"}
	err := c.PutBytes(context.Background(), up, Spec{Path: path, ByteSize: int64(len("bytes"))})
	if err == nil {
		t.Fatal("want the exhausted failure back")
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code() != "storage_rejected_upload" {
		t.Errorf("err = %v, want the last attempt's storage error", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != maxAttempts {
		t.Errorf("attempts = %d, want exactly maxAttempts (%d)", calls, maxAttempts)
	}
	if len(*slept) != maxAttempts-1 {
		t.Errorf("slept %v, want a backoff between each attempt and none after the last", *slept)
	}
}

// Attaching is a PUT because it is idempotent: the artifact ends up under the
// same run however many times it is asked for, so a CI step that runs twice sees
// the same success rather than a spent-token error.
func TestAttachRunIsAnIdempotentPut(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		bodies = append(bodies, string(body))
		mu.Unlock()
		fmt.Fprint(w, `{"slug":"art_x","state":"ready","run":"run_y"}`)
	}))
	t.Cleanup(server.Close)

	c, _ := testClient(server)
	for i := range 2 {
		artifact, err := c.AttachRun(context.Background(), "art_x", "run_y")
		if err != nil {
			t.Fatalf("attach %d = %v", i+1, err)
		}
		if artifact.Run != "run_y" {
			t.Errorf("attach %d returned run %q, want run_y", i+1, artifact.Run)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	// Two attaches, two requests: a success the client retried anyway would mean
	// the run was set more than once for one caller asking once.
	if len(calls) != 2 {
		t.Fatalf("calls = %v, want one per attach", calls)
	}
	for i, call := range calls {
		if call != "PUT /v1/artifacts/art_x/run" {
			t.Errorf("call %d = %q, want PUT /v1/artifacts/art_x/run", i, call)
		}
		if bodies[i] != `{"run":"run_y"}` {
			t.Errorf("body %d = %s, want the run slug", i, bodies[i])
		}
	}
}

// A slug from another workspace reads as not existing, and the fix says so
// without confirming whether it is the artifact or the run that is unknown.
func TestAttachRunOnAForeignSlugIsNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"not_found","message":"No such record."}}`)
	}))
	t.Cleanup(server.Close)

	c, slept := testClient(server)
	_, err := c.AttachRun(context.Background(), "art_someone_else", "run_y")

	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code() != "not_found" {
		t.Fatalf("AttachRun = %v, want not_found", err)
	}
	if !strings.Contains(apiErr.Fix(), "artifact or run") {
		t.Errorf("fix = %q, want it to cover both slugs", apiErr.Fix())
	}
	if len(*slept) != 0 {
		t.Errorf("slept %v — a 404 is not worth retrying", *slept)
	}
}

// Only an upload naming a run reaches run_needs_key — the attach route answers a
// keyless request with a plain 401 — so the fix may name --run, and the CLI must
// not have generalized it into advice that fits neither path well.
func TestRunNeedsKeyFixNamesTheFlagThatCausedIt(t *testing.T) {
	fix := fixFor("run_needs_key", http.StatusUnprocessableEntity)
	if !strings.Contains(fix, "API key") {
		t.Errorf("fix = %q, want it to name the key", fix)
	}
	if !strings.Contains(fix, "--run") {
		t.Errorf("fix = %q, want the flag that caused it", fix)
	}
}

// A slug goes into the URL as one path segment, whatever it contains.
//
// This is not about rejecting bad input — it is that an unescaped slug does not
// fail, it addresses a *different* endpoint. `#` ends the path and makes the
// rest a fragment, so `art_A#/finalization` is a request to `/artifacts/art_A`.
// On a takedown that destroys an artifact other than the one named, which is
// unrecoverable, and no error is reported either way.
func TestASlugCannotEscapeItsPathSegment(t *testing.T) {
	// RequestURI is the raw wire form, which is the only thing that decides which
	// endpoint this reaches. r.URL.Path is already decoded, so reading that would
	// hide the very difference under test.
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.RequestURI)
		// An empty object satisfies every decode here. What is under test is where
		// the request landed, not what came back.
		fmt.Fprint(w, "{}")
	}))
	defer server.Close()

	client := New(server.URL, "krowk_sk_test")
	client.Sleep = func(time.Duration) {}

	// Each of these would otherwise land somewhere other than where it says.
	for _, slug := range []string{"art_A#", "art_A#/finalization", "art_A?x=1", "art_A/../art_B"} {
		if err := client.TakeDownArtifact(context.Background(), slug, ""); err != nil {
			t.Fatalf("takedown %q: %v", slug, err)
		}
	}

	// The run reads matter most of the three: their responses decode into the
	// wrong type without erroring, so a mis-routed one is not a failure but a
	// confident wrong answer. `GET /runs/run_A` answering `runs show` decodes
	// cleanly into a Page, which reads as "this run produced nothing".
	if _, err := client.ShowRun(context.Background(), "run_A#x"); err != nil {
		t.Fatalf("show run: %v", err)
	}
	if _, err := client.ListRunArtifacts(context.Background(), "run_A#", "", 0); err != nil {
		t.Fatalf("list run artifacts: %v", err)
	}

	want := []string{
		"/artifacts/art_A%23",
		"/artifacts/art_A%23%2Ffinalization",
		"/artifacts/art_A%3Fx=1",
		"/artifacts/art_A%2F..%2Fart_B",
		"/runs/run_A%23x",
		"/runs/run_A%23/artifacts",
	}
	if len(paths) != len(want) {
		t.Fatalf("paths = %v", paths)
	}
	for i, w := range want {
		if paths[i] != w {
			t.Errorf("path %d = %q, want %q", i, paths[i], w)
		}
	}
}

// The whole point of the header: a retry of a lost create has to be recognisable
// as the same attempt, so the key is minted once per call and repeated by every
// attempt of it. A fresh key per attempt would make the retry a second create.
func TestIdempotencyKeyIsOneKeyRepeatedAcrossAttempts(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		attempt := len(keys)
		mu.Unlock()
		if attempt < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":{"code":"overloaded","message":"try later"}}`)
			return
		}
		fmt.Fprint(w, `{"slug":"art_x","state":"pending","upload":{"url":"http://x/y"}}`)
	}))
	defer server.Close()

	client, _ := testClient(server)
	if _, err := client.PrepareArtifact(context.Background(), Spec{
		Filename: "a.txt", ContentType: "text/plain", ByteSize: 9,
	}); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 3 {
		t.Fatalf("attempts = %d, want 3", len(keys))
	}
	for i, key := range keys {
		if key == "" {
			t.Fatalf("attempt %d carried no Idempotency-Key", i+1)
		}
		if key != keys[0] {
			t.Errorf("attempt %d sent %q, want the first attempt's %q", i+1, key, keys[0])
		}
	}
}

// Two calls are two attempts, not one retried — so they must not share a key, or
// the second declare would be answered with the first one's artifact.
func TestEachCreateGetsItsOwnKey(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		mu.Unlock()
		fmt.Fprint(w, `{"slug":"art_x","state":"pending","upload":{"url":"http://x/y"}}`)
	}))
	defer server.Close()

	client, _ := testClient(server)
	spec := Spec{Filename: "a.txt", ContentType: "text/plain", ByteSize: 9}
	for range 2 {
		if _, err := client.PrepareArtifact(context.Background(), spec); err != nil {
			t.Fatalf("prepare: %v", err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 2 {
		t.Fatalf("calls = %d, want 2", len(keys))
	}
	if keys[0] == keys[1] {
		t.Error("two separate declares shared one key, so the second would replay the first")
	}
}

// Unguessable rather than merely unique: on a keyless push the key is the only
// thing a retry can present to prove it made the original call, and the registry
// scopes keys by address, so a predictable one hands the declare to anyone sharing
// that address.
func TestIdempotencyKeysDoNotRepeat(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		key, err := newIdempotencyKey()
		if err != nil {
			t.Fatal(err)
		}
		if len(key) != 36 {
			t.Fatalf("key = %q, want a 36-character UUID", key)
		}
		if seen[key] {
			t.Fatalf("key %q repeated", key)
		}
		seen[key] = true
	}
}

// Reading an artifact is not a create, so it carries no key. Sending one would
// mean a GET could be refused as a reuse.
func TestReadsCarryNoIdempotencyKey(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		mu.Unlock()
		fmt.Fprint(w, `{"slug":"art_x","state":"ready"}`)
	}))
	defer server.Close()

	client, _ := testClient(server)
	if _, err := client.ShowArtifact(context.Background(), "art_x"); err != nil {
		t.Fatalf("show: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if keys[0] != "" {
		t.Errorf("a read sent Idempotency-Key %q, want none", keys[0])
	}
}
