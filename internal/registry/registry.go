// Package registry is a stand-in for api.krowk.com until the real one exists.
// It implements the contract krowk.com already publishes — multipart POST,
// digest-derived (idempotent) IDs, machine-readable errors, rate-limit headers
// — so the CLI can be developed and demoed against something honest, and so
// the real registry has a spec to satisfy.
package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	// DefaultLimitBytes matches the free tier the website advertises.
	DefaultLimitBytes int64 = 100 << 20
	dailyUploads            = 100
)

type file struct {
	Filename    string `json:"filename"`
	Bytes       int64  `json:"bytes"`
	ContentType string `json:"content_type"`
}

type artifact struct {
	ID         string          `json:"id"`
	URL        string          `json:"url"`
	PreviewURL string          `json:"preview_url"`
	Bytes      int64           `json:"bytes"`
	ExpiresAt  string          `json:"expires_at"`
	Files      []file          `json:"files"`
	Metadata   json.RawMessage `json:"metadata"`
}

// Handler serves the mock registry. SiteURL is the origin baked into returned
// links; empty means "whatever host the request arrived on".
func Handler(limitBytes int64, siteURL string) http.Handler {
	if limitBytes <= 0 {
		limitBytes = DefaultLimitBytes
	}

	var mu sync.Mutex
	store := map[string]artifact{}

	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/artifacts", func(w http.ResponseWriter, r *http.Request) {
		site := siteURL
		if site == "" {
			site = origin(r)
		}

		// Spool to disk beyond 32 MB rather than holding a video in memory.
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			writeJSON(w, http.StatusBadRequest, nil, map[string]any{
				"error":     "malformed_upload",
				"detail":    err.Error(),
				"fix":       "send a multipart/form-data body with a `file` field",
				"retryable": false,
			})
			return
		}
		defer r.MultipartForm.RemoveAll()

		headers := r.MultipartForm.File["file"]
		if len(headers) == 0 {
			writeJSON(w, http.StatusUnprocessableEntity, nil, map[string]any{
				"error":     "no_file",
				"fix":       "attach at least one file as the multipart field `file`",
				"retryable": false,
			})
			return
		}

		var total int64
		for _, h := range headers {
			total += h.Size
		}

		mu.Lock()
		remaining := max(0, dailyUploads-len(store))
		mu.Unlock()
		rate := rateHeaders(remaining)

		if total > limitBytes {
			writeJSON(w, http.StatusRequestEntityTooLarge, rate, map[string]any{
				"error":       "artifact_too_large",
				"limit_bytes": limitBytes,
				"got_bytes":   total,
				"fix":         fmt.Sprintf("re-encode below %d MB or push frames separately", limitBytes>>20),
				"retryable":   false,
			})
			return
		}

		// The ID is the digest of the bytes, so an agent that retries eleven
		// times gets one artifact and the same link every time.
		digest := sha256.New()
		files := make([]file, 0, len(headers))
		for _, h := range headers {
			f, err := h.Open()
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, rate, map[string]any{
					"error": "upload_unreadable", "detail": err.Error(), "retryable": true,
				})
				return
			}
			_, err = io.Copy(digest, f)
			f.Close()
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, rate, map[string]any{
					"error": "upload_unreadable", "detail": err.Error(), "retryable": true,
				})
				return
			}
			files = append(files, file{
				Filename:    h.Filename,
				Bytes:       h.Size,
				ContentType: contentType(h.Header.Get("Content-Type")),
			})
		}
		id := hex.EncodeToString(digest.Sum(nil))[:7]

		mu.Lock()
		defer mu.Unlock()
		if existing, ok := store[id]; ok {
			writeJSON(w, http.StatusOK, rate, existing)
			return
		}

		metadata := json.RawMessage(r.FormValue("metadata"))
		if !json.Valid(metadata) {
			metadata = json.RawMessage("{}")
		}
		a := artifact{
			ID:         id,
			URL:        fmt.Sprintf("%s/a/%s", site, id),
			PreviewURL: fmt.Sprintf("%s/a/%s/preview.png", site, id),
			Bytes:      total,
			ExpiresAt:  time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339),
			Files:      files,
			Metadata:   metadata,
		}
		store[id] = a
		writeJSON(w, http.StatusCreated, rate, a)
	})

	mux.HandleFunc("GET /v1/artifacts/{id}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		a, ok := store[r.PathValue("id")]
		mu.Unlock()
		if !ok {
			writeJSON(w, http.StatusNotFound, nil, map[string]any{
				"error": "artifact_not_found", "id": r.PathValue("id"),
				"fix": "check the ID", "retryable": false,
			})
			return
		}
		writeJSON(w, http.StatusOK, nil, a)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, nil, map[string]any{
			"error":     "not_found",
			"fix":       "POST " + origin(r) + "/v1/artifacts with a multipart `file` field",
			"retryable": false,
		})
	})

	return mux
}

func rateHeaders(remaining int) map[string]string {
	return map[string]string{
		"X-RateLimit-Limit":     strconv.Itoa(dailyUploads),
		"X-RateLimit-Remaining": strconv.Itoa(remaining),
	}
}

func writeJSON(w http.ResponseWriter, status int, headers map[string]string, body any) {
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}

func origin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func contentType(s string) string {
	if s == "" {
		return "application/octet-stream"
	}
	return s
}
