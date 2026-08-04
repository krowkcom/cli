// Package api talks to the krowk artifact registry.
//
// Uploading is three calls, because the bytes never pass through the registry:
//
//  1. POST /v1/artifacts                        declare the file, get a presigned PUT
//  2. PUT  <presigned url>                      bytes go straight to object storage
//  3. PUT  /v1/artifacts/{slug}/finalization    the registry verifies what landed
//
// Only the first and third are ours; the second is object storage, signed for
// exactly the size, content type and checksum declared in step one. That is why
// the size and digest are computed up front rather than discovered while
// streaming: they are part of what gets signed, so they have to be known before
// the first call is made.
//
// The registry's API is resourceful all the way down, so what would be a verb
// hanging off an artifact is a nested resource instead — the finalization of an
// artifact, the claim on one, the completion of a run. The verb follows from
// whether the call can be repeated: finalizing and completing are idempotent, so
// they are PUTs; claiming spends a one-shot token, so it is a POST.
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

// Authenticated reports whether calls will carry a key. Runs — and so all run
// metadata — are only available to a keyed client.
func (c *Client) Authenticated() bool { return c.Token != "" }

// Upload is where the bytes go, signed for one specific body.
type Upload struct {
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers"`
	ExpiresAt string            `json:"expires_at"`
}

// Artifact is one stored file, as the registry reports it.
type Artifact struct {
	Slug        string `json:"slug"`
	State       string `json:"state"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	ByteSize    int64  `json:"byte_size"`
	Checksum    string `json:"checksum,omitempty"`
	Region      string `json:"region,omitempty"`
	Run         string `json:"run,omitempty"`
	URL         string `json:"url"`
	Markdown    string `json:"markdown,omitempty"`
	ExpiresAt   string `json:"expires_at,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`

	// Upload and NextStep only ever appear on the create response.
	Upload   *Upload `json:"upload,omitempty"`
	NextStep string  `json:"next_step,omitempty"`

	// ClaimToken is shown exactly once, by the call that created an anonymous
	// artifact. It is the only way to keep that artifact past its expiry, so it
	// is carried through to the output rather than dropped here.
	ClaimToken string `json:"claim_token,omitempty"`
}

// Run groups the artifacts one agent run produced, and is where run metadata
// lives — the registry keeps none on an artifact.
type Run struct {
	Slug       string          `json:"slug"`
	Status     string          `json:"status"`
	StartedAt  string          `json:"started_at,omitempty"`
	FinishedAt string          `json:"finished_at,omitempty"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
}

// Error carries a failure flattened into one map, so everything downstream reads
// it the same way regardless of whether it came from the registry, from object
// storage, or from this client.
type Error struct {
	Status int
	Body   map[string]any
}

func (e *Error) Error() string { return e.Code() }

// Code is the stable error identifier, e.g. checksum_mismatch.
func (e *Error) Code() string {
	if s, ok := e.Body["error"].(string); ok && s != "" {
		return s
	}
	return fmt.Sprintf("http_%d", e.Status)
}

// Fix is the human- and agent-readable remedy, when there is one.
func (e *Error) Fix() string {
	s, _ := e.Body["fix"].(string)
	return s
}

// Retryable honours an explicit verdict; absent that, 429 and 5xx are worth
// another attempt and nothing else is.
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

// Spec is a file the client is about to upload, measured and digested so the
// registry can sign an upload URL that only these exact bytes fit.
//
// Path is local and deliberately not serialized: the registry is told the
// basename, never where the file sat on someone's laptop.
type Spec struct {
	Path        string `json:"-"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	ByteSize    int64  `json:"byte_size"`
	Checksum    string `json:"checksum"`
	Run         string `json:"run,omitempty"`
}

// Inspect measures and digests a file. The digest is read off disk up front
// because it is signed into the upload URL — which is what lets storage refuse
// corrupted bytes at the edge rather than after we have paid to store them.
func Inspect(path string) (Spec, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return Spec{}, Fail("file_unreadable",
			"cannot read `"+path+"` — paths resolve from the current directory")
	}
	// The size is signed into the upload URL and the registry requires it above
	// zero, so an empty file cannot be uploaded at all. Saying so here beats a
	// signature error from storage.
	if info.Size() == 0 {
		return Spec{}, Fail("empty_file", "`"+path+"` is empty — there is nothing to upload")
	}

	f, err := os.Open(path)
	if err != nil {
		return Spec{}, Fail("file_unreadable", "cannot read `"+path+"`: "+err.Error())
	}
	defer f.Close()

	digest := sha256.New()
	if _, err := io.Copy(digest, f); err != nil {
		return Spec{}, Fail("file_unreadable", "could not read all of `"+path+"`: "+err.Error())
	}

	return Spec{
		Path:        path,
		Filename:    filepath.Base(path),
		ContentType: ContentType(path),
		ByteSize:    info.Size(),
		Checksum:    hex.EncodeToString(digest.Sum(nil)),
	}, nil
}

// ContentType guesses from the extension, without the charset parameter: the
// type is signed into the upload URL and stored on the artifact, so the shortest
// accurate string is the one worth committing to.
func ContentType(path string) string {
	t := mime.TypeByExtension(filepath.Ext(path))
	if t == "" {
		return "application/octet-stream"
	}
	if i := strings.IndexByte(t, ';'); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	return t
}

// Push runs the whole upload for one file: declare, send the bytes, finalize.
//
// The finalized artifact is what comes back, but the claim token only ever
// appears on the create response — so it is carried across, because losing it
// means an anonymous upload can never be kept.
func (c *Client) Push(ctx context.Context, spec Spec) (*Artifact, error) {
	prepared, err := c.PrepareArtifact(ctx, spec)
	if err != nil {
		return nil, err
	}
	if prepared.Upload == nil || prepared.Upload.URL == "" {
		return nil, Fail("no_upload_url",
			"the registry accepted the artifact but did not say where to put the bytes")
	}

	if err := c.PutBytes(ctx, prepared.Upload, spec); err != nil {
		return nil, err
	}

	final, err := c.FinalizeArtifact(ctx, prepared.Slug)
	if err != nil {
		return nil, err
	}
	final.ClaimToken = prepared.ClaimToken
	return final, nil
}

// PrepareArtifact records the artifact and returns the presigned upload.
func (c *Client) PrepareArtifact(ctx context.Context, spec Spec) (*Artifact, error) {
	var artifact Artifact
	body := map[string]any{"artifact": spec}
	if err := c.call(ctx, http.MethodPost, "/artifacts", body, &artifact); err != nil {
		return nil, err
	}
	return &artifact, nil
}

// FinalizeArtifact confirms the upload landed. A PUT because it is idempotent:
// the artifact ends up in the same state however many times it is asked for, so a
// retry is a success rather than an error.
func (c *Client) FinalizeArtifact(ctx context.Context, slug string) (*Artifact, error) {
	var artifact Artifact
	if err := c.call(ctx, http.MethodPut, "/artifacts/"+slug+"/finalization", nil, &artifact); err != nil {
		return nil, err
	}
	return &artifact, nil
}

// ShowArtifact reads one artifact back. Works without a key: for a keyless
// request the slug is the capability, and it resolves within the anonymous
// workspace.
func (c *Client) ShowArtifact(ctx context.Context, slug string) (*Artifact, error) {
	var artifact Artifact
	if err := c.call(ctx, http.MethodGet, "/artifacts/"+slug, nil, &artifact); err != nil {
		return nil, err
	}
	return &artifact, nil
}

// Page is one page of a workspace's artifacts, newest first. Next carries the
// slug to pass back as before for the following page, and is empty on the last.
type Page struct {
	Artifacts []*Artifact `json:"artifacts"`
	Next      string      `json:"next,omitempty"`
}

// ListArtifacts reads a page of the key's workspace. Unlike the rest of the
// artifact endpoints this one needs a key: keyless requests all share the
// anonymous workspace, so listing it would show everyone's uploads.
//
// before is the cursor from a previous page's Next; limit is clamped by the
// registry rather than here, so asking for more than it serves gets the most it
// serves.
func (c *Client) ListArtifacts(ctx context.Context, before string, limit int) (*Page, error) {
	query := url.Values{}
	if before != "" {
		query.Set("before", before)
	}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	path := "/artifacts"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}

	var page Page
	if err := c.call(ctx, http.MethodGet, path, nil, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// ClaimArtifact spends a claim token to move an anonymous artifact into the
// key's workspace, where it stops expiring.
func (c *Client) ClaimArtifact(ctx context.Context, slug, claimToken string) (*Artifact, error) {
	var artifact Artifact
	body := map[string]any{"claim_token": claimToken}
	if err := c.call(ctx, http.MethodPost, "/artifacts/"+slug+"/claim", body, &artifact); err != nil {
		return nil, err
	}
	return &artifact, nil
}

// CreateRun opens a run to hang artifacts off. Needs a key: a run belongs to a
// workspace, and a keyless upload has none.
func (c *Client) CreateRun(ctx context.Context, metadata any) (*Run, error) {
	var run Run
	body := map[string]any{"run": map[string]any{"metadata": metadata}}
	if err := c.call(ctx, http.MethodPost, "/runs", body, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// FinishRun closes a run. A PUT for the same reason finalizing is one: a CI
// cleanup step that runs twice should get the same success both times, and the
// run keeps the moment it first finished.
func (c *Client) FinishRun(ctx context.Context, slug string) (*Run, error) {
	var run Run
	if err := c.call(ctx, http.MethodPut, "/runs/"+slug+"/completion", nil, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// Service is the descriptor at the API root, which is how doctor tells a
// reachable registry from a reachable something-else.
type Service struct {
	Service  string   `json:"service"`
	Versions []string `json:"versions"`
}

// Root reads the service descriptor. It is the one endpoint needing neither a
// key nor a payload, so it is what a reachability check should ask for.
func (c *Client) Root(ctx context.Context) (*Service, error) {
	// The descriptor sits at the host root, one level above the versioned API.
	url := strings.TrimSuffix(c.BaseURL, "/v1") + "/"

	var service Service
	if err := c.do(ctx, http.MethodGet, url, nil, &service); err != nil {
		return nil, err
	}
	return &service, nil
}

// PutBytes streams the file to object storage using exactly the headers the URL
// was signed for. Anything else — an unsigned header, a different length — and
// the signature no longer matches what arrives.
func (c *Client) PutBytes(ctx context.Context, up *Upload, spec Spec) error {
	method := up.Method
	if method == "" {
		method = http.MethodPut
	}

	var last error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// The body is a file handle, so it has to be reopened for every attempt.
		f, err := os.Open(spec.Path)
		if err != nil {
			return Fail("file_unreadable", "cannot read `"+spec.Path+"`: "+err.Error())
		}

		req, err := http.NewRequestWithContext(ctx, method, up.URL, f)
		if err != nil {
			f.Close()
			return Fail("bad_upload_url", err.Error())
		}
		for k, v := range up.Headers {
			// Content-Length is signed too, but Go will not send it from the
			// header map — it comes off ContentLength below, which is the same
			// size that was signed.
			if strings.EqualFold(k, "Content-Length") {
				continue
			}
			req.Header.Set(k, v)
		}
		req.ContentLength = spec.ByteSize

		err = c.putOnce(req)
		f.Close()
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

func (c *Client) putOnce(req *http.Request) error {
	res, err := c.HTTP.Do(req)
	if err != nil {
		return &Error{Body: map[string]any{
			"error":     "storage_unreachable",
			"detail":    err.Error(),
			"fix":       "the registry issued an upload URL but the bytes could not be sent to it — check the network",
			"retryable": true,
		}}
	}
	defer res.Body.Close()

	if res.StatusCode < 400 {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
		return nil
	}

	// Object storage answers in XML, not our error envelope, so its body is
	// reported as a snippet rather than parsed into a code we would be inventing.
	snippet, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
	return &Error{Status: res.StatusCode, Body: map[string]any{
		"error":     "storage_rejected_upload",
		"detail":    clip(strings.TrimSpace(string(snippet)), 300),
		"fix":       "object storage refused the bytes — most often the file changed after it was measured, or the upload URL expired; retry the upload",
		"retryable": res.StatusCode >= 500,
	}}
}

// call makes one registry request, with retries, decoding into out.
func (c *Client) call(ctx context.Context, method, path string, body any, out any) error {
	return c.do(ctx, method, c.BaseURL+path, body, out)
}

func (c *Client) do(ctx context.Context, method, url string, body any, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return Fail("bad_request", "the request body could not be encoded as JSON")
		}
	}

	var last error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		var reader io.Reader
		if payload != nil {
			reader = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, reader)
		if err != nil {
			return Fail("bad_request", err.Error())
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "application/json")
		if c.Token != "" {
			req.Header.Set("Authorization", "Bearer "+c.Token)
		}

		err = c.doOnce(req, out)
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

func (c *Client) doOnce(req *http.Request, out any) error {
	res, err := c.HTTP.Do(req)
	if err != nil {
		return &Error{Body: map[string]any{
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
		err := responseError(res, payload, readErr)
		// A 401 with no key sent is a missing key, not a rejected one — and
		// "check your token" is unhelpful advice when there is no token to check.
		if err.Code() == "unauthorized" && c.Token == "" {
			err.Body["fix"] = "this endpoint needs an API key — run `krowk auth login --token krowk_sk_...`, or set KROWK_TOKEN"
		}
		return err
	}
	if out == nil {
		return nil
	}
	if readErr != nil || json.Unmarshal(payload, out) != nil {
		return &Error{Status: res.StatusCode, Body: map[string]any{
			"error": "malformed_response",
			"fix":   "the registry answered with a success status and a body this client could not read — check KROWK_API_URL points at the API host, not the website",
		}}
	}
	return nil
}

// registryError is the envelope every krowk API failure arrives in:
//
//	{"error": {"code": "...", "message": "...", "details": {...}}}
type registryError struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

// responseError flattens the registry's envelope into one map. The registry's
// message says what went wrong; fixFor says what to do about it.
func responseError(res *http.Response, payload []byte, readErr error) *Error {
	body := map[string]any{}

	var parsed registryError
	if readErr == nil && json.Unmarshal(payload, &parsed) == nil && parsed.Error.Code != "" {
		body["error"] = parsed.Error.Code
		if parsed.Error.Message != "" {
			body["message"] = parsed.Error.Message
		}
		if len(parsed.Error.Details) > 0 {
			body["details"] = parsed.Error.Details
		}
	} else {
		body["error"] = fmt.Sprintf("http_%d", res.StatusCode)
		// An HTML body is a page, not a payload — most often Rails' own error page
		// for a route that does not exist. Quoting 300 characters of it buries the
		// one fact that matters, so it is named rather than dumped.
		if snippet := strings.TrimSpace(string(payload)); snippet != "" {
			if strings.HasPrefix(snippet, "<") {
				body["detail"] = "the registry answered with an HTML page rather than JSON"
			} else {
				body["detail"] = clip(snippet, 300)
			}
		}
	}

	err := &Error{Status: res.StatusCode, Body: body}
	if retry := res.Header.Get("Retry-After"); retry != "" {
		body["retry_after"] = retry
	}
	if fix := fixFor(err.Code(), res.StatusCode); fix != "" {
		body["fix"] = fix
	}
	if retryable, known := retryableFor(err.Code()); known {
		body["retryable"] = retryable
	}
	return err
}

// fixFor turns the registry's codes into the next thing to actually do.
func fixFor(code string, status int) string {
	switch code {
	case "unauthorized":
		return "the registry rejected the key — check KROWK_TOKEN, or run `krowk auth login --token krowk_sk_...`"
	case "run_needs_key":
		return "attaching an upload to a run needs an API key — authenticate, or upload without --run"
	case "upload_missing":
		return "the bytes had not landed when the upload was finalized — retry the upload"
	case "checksum_mismatch":
		return "the file changed while it was being uploaded — retry the upload"
	case "empty_upload":
		return "what arrived held no bytes — check the file is not being written while it is uploaded"
	case "expired":
		return "this artifact was anonymous and has passed its expiry — upload it again, and claim it with a key to keep it"
	case "storage_unavailable":
		return "object storage is temporarily unreachable — retry shortly"
	case "not_found":
		// Scoping, not permission: the registry answers "no such record" for
		// another workspace's slug rather than "forbidden", which would confirm it
		// exists. So a wrong slug and someone else's slug read the same.
		return "no such artifact or run in this workspace — check the slug, and that the key matches the workspace it was uploaded to"
	case "parameter_missing", "invalid":
		return "" // the registry's own message names the parameter
	}
	if status >= 500 {
		return "the registry failed on its side — retry, and report it if it persists"
	}
	if status == http.StatusNotFound {
		return "the registry has no such endpoint — check KROWK_API_URL names the API host and version, since it routes by hostname and the wrong host answers 404"
	}
	return ""
}

// retryableFor overrides the status-based default where the code knows better.
// The second return says whether this code has an opinion at all.
func retryableFor(code string) (bool, bool) {
	switch code {
	case "upload_missing", "storage_unavailable":
		return true, true
	// Nothing about retrying these changes the answer, and an agent that keeps
	// trying is worse than one that stops.
	case "invalid", "parameter_missing", "unauthorized", "not_found", "expired",
		"checksum_mismatch", "empty_upload", "run_needs_key":
		return false, true
	}
	return false, false
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

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
