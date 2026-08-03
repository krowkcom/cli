// Package api talks to the krowk artifact registry.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is overridden by KROWK_API_URL.
	DefaultBaseURL = "https://api.krowk.com/v1"
	maxAttempts    = 3
)

// Client is safe for a single CLI invocation; it holds no state between calls.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	// Sleep is swapped out in tests so backoff does not cost wall clock.
	Sleep func(time.Duration)
}

// New builds a client against baseURL, falling back to the public registry.
func New(baseURL, token string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 5 * time.Minute},
		Sleep:   time.Sleep,
	}
}

// File is one uploaded file as the registry reports it back.
type File struct {
	Filename    string `json:"filename"`
	Bytes       int64  `json:"bytes"`
	ContentType string `json:"content_type,omitempty"`
}

// Artifact is a successful upload.
type Artifact struct {
	ID                 string          `json:"id"`
	URL                string          `json:"url"`
	PreviewURL         string          `json:"preview_url,omitempty"`
	Bytes              int64           `json:"bytes,omitempty"`
	ExpiresAt          string          `json:"expires_at,omitempty"`
	Files              []File          `json:"files,omitempty"`
	Metadata           json.RawMessage `json:"metadata,omitempty"`
	RateLimitRemaining string          `json:"rate_limit_remaining,omitempty"`
}

// Error carries the registry's machine-readable failure body verbatim, so the
// caller can print the limit, the actual value and the fix without guessing.
type Error struct {
	Status int
	Body   map[string]any
}

func (e *Error) Error() string { return e.Code() }

// Code is the stable error identifier, e.g. artifact_too_large.
func (e *Error) Code() string {
	if s, ok := e.Body["error"].(string); ok && s != "" {
		return s
	}
	return fmt.Sprintf("http_%d", e.Status)
}

// Fix is the human- and agent-readable remedy, when the registry offers one.
func (e *Error) Fix() string {
	s, _ := e.Body["fix"].(string)
	return s
}

// Retryable honours the server's own verdict; absent that, 429 and 5xx are
// worth another attempt and nothing else is.
func (e *Error) Retryable() bool {
	if b, ok := e.Body["retryable"].(bool); ok {
		return b
	}
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// Fail builds a client-side error in the same shape as a server-side one.
func Fail(code, fix string) *Error {
	return &Error{Body: map[string]any{"error": code, "fix": fix, "retryable": false}}
}

// CreateArtifact uploads files with their metadata and returns the canonical URL.
func (c *Client) CreateArtifact(ctx context.Context, files []string, metadata any) (*Artifact, error) {
	meta, err := json.Marshal(metadata)
	if err != nil {
		return nil, Fail("bad_metadata", "the metadata could not be encoded as JSON")
	}

	var last error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// The body streams off disk, so it must be rebuilt for every attempt.
		body, contentType := multipartBody(files, meta)

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/artifacts", body)
		if err != nil {
			return nil, Fail("bad_request", err.Error())
		}
		req.Header.Set("Content-Type", contentType)
		if c.Token != "" {
			req.Header.Set("Authorization", "Bearer "+c.Token)
		}

		artifact, err := c.do(req)
		if err == nil {
			return artifact, nil
		}
		last = err

		var apiErr *Error
		if !errors.As(err, &apiErr) || !apiErr.Retryable() || attempt == maxAttempts {
			return nil, err
		}
		c.Sleep(backoff(apiErr, attempt))
	}
	return nil, last
}

func (c *Client) do(req *http.Request) (*Artifact, error) {
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, &Error{Body: map[string]any{
			"error":     "network_unreachable",
			"endpoint":  req.URL.String(),
			"detail":    err.Error(),
			"fix":       "cannot reach " + c.BaseURL + " — check the network, or point KROWK_API_URL at a reachable registry",
			"retryable": false,
		}}
	}
	defer res.Body.Close()

	payload, readErr := io.ReadAll(io.LimitReader(res.Body, 1<<20))

	if res.StatusCode >= 400 {
		body := map[string]any{}
		if readErr != nil || json.Unmarshal(payload, &body) != nil || body["error"] == nil {
			body = map[string]any{
				"error": fmt.Sprintf("http_%d", res.StatusCode),
				"fix":   "the registry did not return a JSON error — check KROWK_API_URL points at the API, not the website",
			}
		}
		err := &Error{Status: res.StatusCode, Body: body}
		if retry := res.Header.Get("Retry-After"); retry != "" {
			err.Body["retry_after"] = retry
		}
		return nil, err
	}

	var artifact Artifact
	if readErr != nil || json.Unmarshal(payload, &artifact) != nil {
		return nil, &Error{Status: res.StatusCode, Body: map[string]any{
			"error": "malformed_response",
			"fix":   "the registry returned a success status with a body that is not an artifact",
		}}
	}
	artifact.RateLimitRemaining = res.Header.Get("X-RateLimit-Remaining")
	return &artifact, nil
}

// backoff honours Retry-After when the registry sends one.
func backoff(e *Error, attempt int) time.Duration {
	if v, ok := e.Body["retry_after"].(string); ok {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return time.Duration(1<<attempt) * 250 * time.Millisecond
}

// multipartBody streams the files rather than buffering them, so a 2 GB video
// costs a pipe rather than 2 GB of resident memory.
func multipartBody(files []string, metadata []byte) (io.Reader, string) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	go func() {
		// LIFO: the writer's closing boundary lands before the pipe shuts.
		var err error
		defer func() { pw.CloseWithError(err) }()
		defer mw.Close()

		for _, path := range files {
			var f *os.File
			if f, err = os.Open(path); err != nil {
				return
			}
			var w io.Writer
			if w, err = createFilePart(mw, filepath.Base(path)); err != nil {
				f.Close()
				return
			}
			_, err = io.Copy(w, f)
			f.Close()
			if err != nil {
				return
			}
		}
		err = mw.WriteField("metadata", string(metadata))
	}()

	return pr, mw.FormDataContentType()
}

var quoteEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

// createFilePart is multipart.CreateFormFile with a real content type, so the
// registry does not have to sniff every upload.
func createFilePart(mw *multipart.Writer, filename string) (io.Writer, error) {
	contentType := mime.TypeByExtension(filepath.Ext(filename))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, quoteEscaper.Replace(filename)))
	h.Set("Content-Type", contentType)
	return mw.CreatePart(h)
}
