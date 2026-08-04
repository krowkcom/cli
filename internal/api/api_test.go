package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// errorAs is errors.As, named so the assertions below read as assertions.
func errorAs(err error, target any) bool { return errors.As(err, target) }

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDescribeIsStableAndSensitive(t *testing.T) {
	dir := t.TempDir()
	a := write(t, dir, "before.png", "one")
	b := write(t, dir, "after.png", "two")

	manifest, key, err := Describe([]string{a, b})
	if err != nil {
		t.Fatal(err)
	}

	// Same inputs, same key — that is what makes a rerun free.
	if _, again, _ := Describe([]string{a, b}); again != key {
		t.Errorf("key is not stable: %q then %q", key, again)
	}
	// Order is part of the identity, because it decides which bytes land under
	// which name.
	if _, swapped, _ := Describe([]string{b, a}); swapped == key {
		t.Error("swapping the files produced the same key")
	}

	if len(manifest) != 2 {
		t.Fatalf("manifest has %d entries, want 2", len(manifest))
	}
	sum := sha256.Sum256([]byte("one"))
	if manifest[0].Digest != hex.EncodeToString(sum[:]) {
		t.Errorf("digest = %q, want the sha256 of the contents", manifest[0].Digest)
	}
	if manifest[0].Bytes != 3 || manifest[0].Filename != "before.png" {
		t.Errorf("manifest[0] = %+v", manifest[0])
	}
	if manifest[0].ContentType != "image/png" {
		t.Errorf("content type = %q, want image/png", manifest[0].ContentType)
	}
}

func TestDescribeRejectsWhatItCannotRead(t *testing.T) {
	if _, _, err := Describe(nil); err == nil {
		t.Error("no files should be an error")
	}
	_, _, err := Describe([]string{filepath.Join(t.TempDir(), "absent.png")})
	if err == nil || err.(*Error).Code() != "file_unreadable" {
		t.Errorf("err = %v, want file_unreadable", err)
	}
	if _, _, err := Describe([]string{t.TempDir()}); err == nil {
		t.Error("a directory is not a regular file")
	}
}

// step records one leg of the handshake.
type step struct {
	name string
	auth string
	key  string
}

// stubRegistry is the smallest thing that speaks the handshake, so the client's
// side of it can be asserted without the full mock.
func stubRegistry(t *testing.T, steps *[]step, mu *sync.Mutex, finalizeHost func() string) *httptest.Server {
	t.Helper()

	var srv *httptest.Server
	mux := http.NewServeMux()

	record := func(name string, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		*steps = append(*steps, step{
			name: name,
			auth: r.Header.Get("Authorization"),
			key:  r.Header.Get("Idempotency-Key"),
		})
	}

	mux.HandleFunc("POST /v1/artifacts", func(w http.ResponseWriter, r *http.Request) {
		record("begin", r)
		var body beginRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("begin body: %v", err)
		}
		if len(body.Files) != 1 || body.Files[0].Digest == "" {
			t.Errorf("manifest = %+v, want one file with a digest", body.Files)
		}
		targets := []UploadTarget{{
			Filename: body.Files[0].Filename,
			Method:   http.MethodPut,
			URL:      srv.URL + "/blobs/tok",
			Headers:  map[string]string{"Content-Type": body.Files[0].ContentType},
		}}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(beginResponse{
			ID:          "abc1234",
			Uploads:     targets,
			FinalizeURL: finalizeHost() + "/v1/artifacts/abc1234/finalize",
		})
	})

	mux.HandleFunc("PUT /blobs/tok", func(w http.ResponseWriter, r *http.Request) {
		record("put", r)
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("POST /v1/artifacts/{id}/finalize", func(w http.ResponseWriter, r *http.Request) {
		record("finalize", r)
		w.Header().Set("X-RateLimit-Remaining", "97")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Artifact{
			ID:    r.PathValue("id"),
			URL:   "https://krowk.com/a/" + r.PathValue("id"),
			Bytes: 3,
		})
	})

	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestHandshakeRunsThreeStepsAndKeepsTheTokenOffStorage(t *testing.T) {
	var mu sync.Mutex
	var steps []step

	srv := stubRegistry(t, &steps, &mu, func() string { return "" }) // relative finalize_url
	client := New(srv.URL+"/v1", "krk_secret")
	client.Sleep = func(time.Duration) {}

	file := write(t, t.TempDir(), "shot.png", "one")
	artifact, err := client.CreateArtifact(context.Background(), []string{file}, map[string]string{"repo": "acme/x"})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if artifact.URL != "https://krowk.com/a/abc1234" {
		t.Errorf("URL = %q", artifact.URL)
	}
	if artifact.RateLimitRemaining != "97" {
		t.Errorf("rate limit = %q, want it carried off the finalize response", artifact.RateLimitRemaining)
	}

	if len(steps) != 3 || steps[0].name != "begin" || steps[1].name != "put" || steps[2].name != "finalize" {
		t.Fatalf("steps = %+v, want begin -> put -> finalize", steps)
	}
	// The presigned URL carries its own authorisation. Sending the API token to
	// whatever host it names would hand the key to a third party.
	if steps[1].auth != "" {
		t.Errorf("the blob PUT carried Authorization %q", steps[1].auth)
	}
	if steps[0].auth != "Bearer krk_secret" || steps[2].auth != "Bearer krk_secret" {
		t.Errorf("the API steps should be authenticated: %+v", steps)
	}
	// One key for the whole handshake, so either end can dedupe on it.
	if steps[0].key == "" || steps[0].key != steps[2].key {
		t.Errorf("idempotency keys = %q and %q", steps[0].key, steps[2].key)
	}
}

func TestFinalizeOnAnotherHostIsRefused(t *testing.T) {
	var elsewhere int
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere++
		w.WriteHeader(http.StatusOK)
	}))
	defer other.Close()

	var mu sync.Mutex
	var steps []step
	srv := stubRegistry(t, &steps, &mu, func() string { return other.URL })

	client := New(srv.URL+"/v1", "krk_secret")
	client.Sleep = func(time.Duration) {}

	file := write(t, t.TempDir(), "shot.png", "one")
	_, err := client.CreateArtifact(context.Background(), []string{file}, nil)
	if err == nil {
		t.Fatal("a finalize_url on a foreign host should be refused")
	}
	if code := err.(*Error).Code(); code != "untrusted_endpoint" {
		t.Errorf("error = %q, want untrusted_endpoint", code)
	}
	if elsewhere != 0 {
		t.Errorf("the client sent %d request(s) to the foreign host", elsewhere)
	}
}

func TestUploadURLMustBeHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(beginResponse{
			ID:          "abc1234",
			Uploads:     []UploadTarget{{URL: "file:///etc/passwd"}},
			FinalizeURL: "/v1/artifacts/abc1234/finalize",
		})
	}))
	defer srv.Close()

	client := New(srv.URL+"/v1", "")
	client.Sleep = func(time.Duration) {}

	file := write(t, t.TempDir(), "shot.png", "one")
	_, err := client.CreateArtifact(context.Background(), []string{file}, nil)
	if err == nil || err.(*Error).Code() != "malformed_response" {
		t.Errorf("err = %v, want malformed_response for a file:// upload target", err)
	}
}

func TestAlreadyCompleteSkipsTheUpload(t *testing.T) {
	var puts int
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/artifacts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(beginResponse{
			ID:       "abc1234",
			Complete: true,
			Artifact: &Artifact{ID: "abc1234", URL: "https://krowk.com/a/abc1234"},
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { puts++ })

	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := New(srv.URL+"/v1", "")
	file := write(t, t.TempDir(), "shot.png", "one")

	artifact, err := client.CreateArtifact(context.Background(), []string{file}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.URL != "https://krowk.com/a/abc1234" {
		t.Errorf("URL = %q", artifact.URL)
	}
	if puts != 0 {
		t.Errorf("a finished upload should not send bytes again, got %d request(s)", puts)
	}
}

func TestRetryAfterAcceptsBothSpellings(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"5", 5 * time.Second, true},
		{" 7 ", 7 * time.Second, true},
		// The spec allows an absolute date, which Atoi alone would drop.
		{now.Add(30 * time.Second).UTC().Format(http.TimeFormat), 30 * time.Second, true},
		// A registry must not be able to wedge the CLI for a week.
		{"604800", 60 * time.Second, true},
		{now.Add(72 * time.Hour).UTC().Format(http.TimeFormat), 60 * time.Second, true},
		// Already past, or nonsense: fall back to exponential backoff.
		{now.Add(-time.Hour).UTC().Format(http.TimeFormat), 0, false},
		{"0", 0, false},
		{"-3", 0, false},
		{"soon", 0, false},
		{"", 0, false},
	} {
		got, ok := retryAfter(tc.in, now)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("retryAfter(%q) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestVerifyKeyReportsScopes(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Header().Set("X-RateLimit-Remaining", "42")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Key{
			Valid:     true,
			KeyID:     "key_7f3a",
			Workspace: "acme",
			Scopes:    []string{"artifacts:read", ScopeWrite},
		})
	}))
	defer srv.Close()

	client := New(srv.URL+"/v1", "krk_secret")
	key, err := client.VerifyKey(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer krk_secret" {
		t.Errorf("Authorization = %q", auth)
	}
	if key.KeyID != "key_7f3a" || key.Workspace != "acme" {
		t.Errorf("key = %+v", key)
	}
	if !key.HasScope(ScopeWrite) || key.HasScope("artifacts:delete") {
		t.Errorf("scopes = %v", key.Scopes)
	}
	if key.RateLimitRemaining != "42" {
		t.Errorf("remaining = %q, want it off the header", key.RateLimitRemaining)
	}
}

// A 200 is not the same as a yes.
func TestVerifyKeyTreatsValidFalseAsRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Key{Valid: false})
	}))
	defer srv.Close()

	_, err := New(srv.URL+"/v1", "krk_secret").VerifyKey(context.Background())
	var apiErr *Error
	if !errorAs(err, &apiErr) || apiErr.Code() != "invalid_key" {
		t.Fatalf("err = %v, want invalid_key", err)
	}
	// The verdict rode in on a 200; carrying that status is what lets doctor
	// tell "the registry said no" apart from "nothing answered at all".
	if apiErr.Status != http.StatusOK {
		t.Errorf("status = %d, want the 200 the verdict arrived with", apiErr.Status)
	}
}

func TestVerifyKeyPassesTheRegistrysRejectionThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"key_revoked","fix":"issue a new key","retryable":false}`)
	}))
	defer srv.Close()

	_, err := New(srv.URL+"/v1", "krk_secret").VerifyKey(context.Background())
	var apiErr *Error
	if !errorAs(err, &apiErr) || apiErr.Code() != "key_revoked" || apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("err = %v, want the registry's own key_revoked", err)
	}
	if apiErr.Fix() != "issue a new key" {
		t.Errorf("fix = %q", apiErr.Fix())
	}
}

func TestRetryableStepIsRetriedThenSucceeds(t *testing.T) {
	var attempts int
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/artifacts", func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", "0")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":"rate_limited","retryable":true}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(beginResponse{
			ID:       "abc1234",
			Complete: true,
			Artifact: &Artifact{ID: "abc1234", URL: "https://krowk.com/a/abc1234"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var slept int
	client := New(srv.URL+"/v1", "")
	client.Sleep = func(time.Duration) { slept++ }

	file := write(t, t.TempDir(), "shot.png", "one")
	if _, err := client.CreateArtifact(context.Background(), []string{file}, nil); err != nil {
		t.Fatalf("a 429 with retryable:true should be retried: %v", err)
	}
	if attempts != 2 || slept != 1 {
		t.Errorf("attempts = %d, sleeps = %d, want 2 and 1", attempts, slept)
	}
}
