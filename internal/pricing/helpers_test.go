package pricing

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const testETag = `"test-etag-1"`

// testServer answers the first GET with body+ETag and every later GET with a
// 304, recording the conditional header it saw.
func testServer(t *testing.T, hits *int, lastIfNoneMatch *string, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		if inm := r.Header.Get("If-None-Match"); inm != "" {
			*lastIfNoneMatch = inm
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if r.URL.Path != "/" && r.URL.Path != "/api.json" {
			t.Errorf("refresh fetched %q, want the models.dev URL path", r.URL.Path)
		}
		w.Header().Set("ETag", testETag)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

func testServerStatus(t *testing.T, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
}

func testServerBody(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
}

func testServerSlow(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(5 * time.Second):
			_, _ = w.Write([]byte(`{}`))
		}
	}))
}
