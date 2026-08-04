// Package api talks to the krowk artifact registry.
//
// An upload is a three-step handshake: declare the files, PUT their bytes to
// the presigned URLs the registry hands back, then finalize. Every step carries
// the same idempotency key, derived from the bytes themselves, so an agent that
// retries — or crashes and runs the whole command again — converges on one
// artifact and one link instead of littering the registry with duplicates.
package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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

// Key is what the registry says about the token it was called with. Verifying
// beats guessing: a key can be revoked, expired, or simply scoped for reading,
// and none of that is visible from the token string.
type Key struct {
	Valid              bool     `json:"valid"`
	KeyID              string   `json:"key_id,omitempty"`
	Workspace          string   `json:"workspace,omitempty"`
	Scopes             []string `json:"scopes,omitempty"`
	ExpiresAt          string   `json:"expires_at,omitempty"`
	RateLimitRemaining string   `json:"rate_limit_remaining,omitempty"`
	// Status is the HTTP status the verification answered with. Diagnostics
	// print what actually arrived rather than assuming 200; the send path
	// accepts any 2xx. Transport detail, not part of the key itself.
	Status int `json:"-"`
}

// HasScope reports whether the key carries a named scope.
func (k *Key) HasScope(scope string) bool {
	return slices.Contains(k.Scopes, scope)
}

// ScopeWrite is what an upload needs.
const ScopeWrite = "artifacts:write"

// VerifyKey asks the registry to confirm the token and report its scopes. It
// sends whatever token the client holds, including none, so the caller can tell
// "no key" from "rejected key" from "registry unreachable".
func (c *Client) VerifyKey(ctx context.Context) (*Key, error) {
	endpoint, err := c.sameOrigin(c.BaseURL + "/keys/verify")
	if err != nil {
		return nil, err
	}

	var key Key
	var status int
	err = c.retry(ctx, func() error {
		req, err := c.request(ctx, http.MethodPost, endpoint, strings.NewReader("{}"), "")
		if err != nil {
			return err
		}
		key = Key{}
		res, err := c.decode(req, &key)
		if err != nil {
			return err
		}
		key.RateLimitRemaining = res.Header.Get("X-RateLimit-Remaining")
		key.Status = res.Status
		status = res.Status
		return nil
	})
	if err != nil {
		return nil, err
	}
	// A 200 saying valid:false is still a rejection; surface it as one, carrying
	// the status it arrived with so the caller can tell it apart from an error
	// formed before any HTTP exchange.
	if !key.Valid {
		return nil, &Error{Status: status, Body: map[string]any{
			"error":     "invalid_key",
			"fix":       "the registry does not recognise this key — run `krowk auth login --token krk_...` with a current one",
			"retryable": false,
		}}
	}
	return &key, nil
}

// ManifestFile declares one file before any bytes are sent. Digest lets the
// registry verify each blob on arrival rather than trusting the client.
type ManifestFile struct {
	Filename    string `json:"filename"`
	Bytes       int64  `json:"bytes"`
	ContentType string `json:"content_type"`
	Digest      string `json:"digest"`
}

// beginRequest opens the handshake.
type beginRequest struct {
	IdempotencyKey string         `json:"idempotency_key"`
	Files          []ManifestFile `json:"files"`
	Metadata       any            `json:"metadata,omitempty"`
}

// UploadTarget is one presigned destination. Headers are whatever the storage
// host requires; the API token deliberately never rides along.
type UploadTarget struct {
	Filename string            `json:"filename"`
	Method   string            `json:"method"`
	URL      string            `json:"url"`
	Headers  map[string]string `json:"headers,omitempty"`
}

// beginResponse is the registry's answer to the manifest. Complete means this
// idempotency key was already finalized and Artifact is the original result.
type beginResponse struct {
	ID          string         `json:"id"`
	Uploads     []UploadTarget `json:"uploads"`
	FinalizeURL string         `json:"finalize_url"`
	Complete    bool           `json:"complete"`
	Artifact    *Artifact      `json:"artifact"`
}

// CreateArtifact runs the whole handshake and returns the canonical URL.
func (c *Client) CreateArtifact(ctx context.Context, files []string, metadata any) (*Artifact, error) {
	manifest, key, err := Describe(files)
	if err != nil {
		return nil, err
	}

	begin, err := c.begin(ctx, beginRequest{IdempotencyKey: key, Files: manifest, Metadata: metadata})
	if err != nil {
		return nil, err
	}
	// An earlier attempt already carried this exact upload all the way through.
	if begin.Complete && begin.Artifact != nil {
		return begin.Artifact, nil
	}
	if err := c.putAll(ctx, files, manifest, begin.Uploads); err != nil {
		return nil, err
	}
	return c.finalize(ctx, key, begin.FinalizeURL)
}

// Describe stats and hashes the files, returning the manifest and the
// idempotency key. The key folds every filename, size and content digest
// together in order, so the same push always derives the same key on any
// machine, and a swapped pair of filenames derives a different one.
func Describe(paths []string) ([]ManifestFile, string, error) {
	if len(paths) == 0 {
		return nil, "", Fail("no_file", "pass at least one path: `krowk uploads create screenshot.png`")
	}

	key := sha256.New()
	manifest := make([]ManifestFile, 0, len(paths))

	for _, path := range paths {
		name := filepath.Base(path)

		f, err := os.Open(path)
		if err != nil {
			return nil, "", Fail("file_unreadable",
				"cannot read `"+path+"` — paths resolve from the current directory")
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			f.Close()
			return nil, "", Fail("file_unreadable", "`"+path+"` is not a regular file")
		}
		sum := sha256.New()
		size, err := io.Copy(sum, f)
		f.Close()
		if err != nil {
			return nil, "", Fail("file_unreadable", "could not read all of `"+path+"`: "+err.Error())
		}
		digest := hex.EncodeToString(sum.Sum(nil))

		// NUL-delimited so "a" + "bc" cannot collide with "ab" + "c".
		fmt.Fprintf(key, "%s\x00%d\x00%s\x00", name, size, digest)

		manifest = append(manifest, ManifestFile{
			Filename:    name,
			Bytes:       size,
			ContentType: ContentType(name),
			Digest:      digest,
		})
	}
	return manifest, hex.EncodeToString(key.Sum(nil)), nil
}

// ContentType guesses from the extension so the registry never has to sniff.
func ContentType(filename string) string {
	if t := mime.TypeByExtension(filepath.Ext(filename)); t != "" {
		return t
	}
	return "application/octet-stream"
}

func (c *Client) begin(ctx context.Context, body beginRequest) (*beginResponse, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, Fail("bad_metadata", "the metadata could not be encoded as JSON")
	}
	endpoint, err := c.sameOrigin(c.BaseURL + "/artifacts")
	if err != nil {
		return nil, err
	}

	var out beginResponse
	err = c.retry(ctx, func() error {
		req, err := c.request(ctx, http.MethodPost, endpoint, bytes.NewReader(payload), body.IdempotencyKey)
		if err != nil {
			return err
		}
		out = beginResponse{}
		_, err = c.decode(req, &out)
		return err
	})
	if err != nil {
		return nil, err
	}

	if out.Complete {
		if out.Artifact == nil || out.Artifact.URL == "" {
			return nil, malformed("the registry reported this upload as already complete but returned no artifact")
		}
		return &out, nil
	}
	if len(out.Uploads) != len(body.Files) {
		return nil, malformed(fmt.Sprintf(
			"declared %d file(s) but the registry returned %d upload target(s)", len(body.Files), len(out.Uploads)))
	}
	if out.FinalizeURL == "" {
		return nil, malformed("the registry returned upload targets but no finalize_url")
	}
	return &out, nil
}

func (c *Client) putAll(ctx context.Context, paths []string, manifest []ManifestFile, targets []UploadTarget) error {
	for i, target := range targets {
		// Targets come back in manifest order; a filename that disagrees means
		// the bytes would land under the wrong name.
		if target.Filename != "" && target.Filename != manifest[i].Filename {
			return malformed("upload target " + strconv.Itoa(i) + " is for `" + target.Filename +
				"` where `" + manifest[i].Filename + "` was declared")
		}
		if err := c.put(ctx, paths[i], manifest[i], target); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) put(ctx context.Context, path string, declared ManifestFile, target UploadTarget) error {
	endpoint, err := storageOrigin(target.URL)
	if err != nil {
		return err
	}
	method := target.Method
	if method == "" {
		method = http.MethodPut
	}

	return c.retry(ctx, func() error {
		// Reopened per attempt: the body streams off disk and cannot be rewound
		// once a failed attempt has drained it.
		f, err := os.Open(path)
		if err != nil {
			return Fail("file_unreadable", "cannot read `"+path+"` — it went away mid-upload")
		}
		defer f.Close()

		req, err := http.NewRequestWithContext(ctx, method, endpoint, f)
		if err != nil {
			return Fail("bad_request", err.Error())
		}
		// A presigned URL carries its own authorisation and may point at any
		// storage host, so the API token must not be attached here.
		for k, v := range target.Headers {
			req.Header.Set(k, v)
		}
		if req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", declared.ContentType)
		}
		req.ContentLength = declared.Bytes

		_, err = c.send(req)
		return err
	})
}

func (c *Client) finalize(ctx context.Context, key, finalizeURL string) (*Artifact, error) {
	endpoint, err := c.sameOrigin(finalizeURL)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(map[string]string{"idempotency_key": key})
	if err != nil {
		return nil, Fail("bad_request", err.Error())
	}

	var artifact Artifact
	err = c.retry(ctx, func() error {
		req, err := c.request(ctx, http.MethodPost, endpoint, bytes.NewReader(payload), key)
		if err != nil {
			return err
		}
		artifact = Artifact{}
		res, err := c.decode(req, &artifact)
		if err != nil {
			return err
		}
		artifact.RateLimitRemaining = res.Header.Get("X-RateLimit-Remaining")
		return nil
	})
	if err != nil {
		return nil, err
	}
	if artifact.URL == "" {
		return nil, malformed("the upload finalized without a canonical URL")
	}
	return &artifact, nil
}

// request builds an authenticated JSON request against the API host.
func (c *Client) request(ctx context.Context, method, endpoint string, body io.Reader, key string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, Fail("bad_request", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	return req, nil
}

// response is one completed HTTP exchange that did not fail.
type response struct {
	Status int
	Header http.Header
	Body   []byte
}

func (c *Client) send(req *http.Request) (*response, error) {
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
		apiErr := &Error{Status: res.StatusCode, Body: body}
		if retry := res.Header.Get("Retry-After"); retry != "" {
			apiErr.Body["retry_after"] = retry
		}
		return nil, apiErr
	}
	if readErr != nil {
		return nil, &Error{Status: res.StatusCode, Body: map[string]any{
			"error": "truncated_response", "detail": readErr.Error(), "retryable": true,
		}}
	}
	return &response{Status: res.StatusCode, Header: res.Header, Body: payload}, nil
}

func (c *Client) decode(req *http.Request, out any) (*response, error) {
	res, err := c.send(req)
	if err != nil {
		return nil, err
	}
	if json.Unmarshal(res.Body, out) != nil {
		return nil, &Error{Status: res.Status, Body: map[string]any{
			"error":     "malformed_response",
			"fix":       "the registry returned a success status with a body this client cannot read",
			"retryable": false,
		}}
	}
	return res, nil
}

// retry runs do until it succeeds, the registry says stop, or the attempts run out.
func (c *Client) retry(ctx context.Context, do func() error) error {
	var last error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return Fail("cancelled", err.Error())
		}
		err := do()
		if err == nil {
			return nil
		}
		last = err

		var apiErr *Error
		if !errors.As(err, &apiErr) || !apiErr.Retryable() || attempt == maxAttempts {
			return err
		}
		c.Sleep(backoff(apiErr, attempt))
	}
	return last
}

// backoff honours Retry-After when the registry sends one, in either spelling
// the HTTP spec allows — a delay in seconds, or an absolute date.
func backoff(e *Error, attempt int) time.Duration {
	if v, ok := e.Body["retry_after"].(string); ok {
		if d, ok := retryAfter(v, time.Now()); ok {
			return d
		}
	}
	return time.Duration(1<<attempt) * 250 * time.Millisecond
}

// retryAfter caps what the registry can ask for: a header saying "come back in
// a week" must not wedge the CLI for a week.
func retryAfter(v string, now time.Time) (time.Duration, bool) {
	const maxWait = 60 * time.Second

	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
		if secs <= 0 {
			return 0, false
		}
		return min(time.Duration(secs)*time.Second, maxWait), true
	}
	at, err := http.ParseTime(strings.TrimSpace(v))
	if err != nil {
		return 0, false
	}
	if d := at.Sub(now); d > 0 {
		return min(d, maxWait), true
	}
	return 0, false
}

// sameOrigin resolves raw against the API base and refuses to take the token
// anywhere else. finalize_url arrives in a response body, so without this a
// compromised or buggy registry could point the authenticated step at a host
// of its choosing.
func (c *Client) sameOrigin(raw string) (string, error) {
	base, err := url.Parse(c.BaseURL)
	if err != nil || base.Host == "" {
		return "", Fail("bad_api_url",
			"KROWK_API_URL is not an absolute URL: "+c.BaseURL)
	}
	u, err := base.Parse(raw)
	if err != nil {
		return "", malformed("the registry returned an unusable URL: " + raw)
	}
	if u.Scheme != base.Scheme || u.Host != base.Host {
		return "", Fail("untrusted_endpoint",
			"the registry pointed an authenticated step at "+u.Scheme+"://"+u.Host+
				" instead of "+base.Scheme+"://"+base.Host+" — refusing to send the API token there")
	}
	return u.String(), nil
}

// storageOrigin accepts any http(s) host, because that is the whole point of a
// presigned URL, but nothing else — a file:// or data: target would make the
// client read or leak something local.
func storageOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", malformed("the registry returned an upload URL that is not http(s): " + raw)
	}
	return u.String(), nil
}

func malformed(detail string) *Error {
	return &Error{Body: map[string]any{
		"error":     "malformed_response",
		"detail":    detail,
		"fix":       "the registry is not speaking the upload handshake this client expects",
		"retryable": false,
	}}
}
