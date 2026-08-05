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
