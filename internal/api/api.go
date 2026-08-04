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
	"net"
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
	// DevBaseURL is where `krowk registry serve` listens, and what --dev points
	// at, so testing against a local registry needs no environment plumbing.
	DevBaseURL  = "http://localhost:8787/v1"
	maxAttempts = 3
)

// Client is safe for a single CLI invocation; it holds no state between calls.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	// Sleep is swapped out in tests so backoff does not cost wall clock.
	Sleep func(time.Duration)
}

// BaseURLFor picks which registry to talk to. dev is an explicit request — a
// command-line flag — so it wins over an ambient environment variable;
// KROWK_API_URL then beats KROWK_DEV because it names a specific target.
func BaseURLFor(dev bool, env func(string) string) string {
	switch {
	case dev:
		return DevBaseURL
	case env("KROWK_API_URL") != "":
		return env("KROWK_API_URL")
	case Truthy(env("KROWK_DEV")):
		return DevBaseURL
	}
	return DefaultBaseURL
}

// Truthy reads the spellings people actually type into an environment variable.
func Truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// New builds a client against baseURL, falling back to the public registry.
func New(baseURL, token string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	c := &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		Sleep:   time.Sleep,
	}
	// The upload boundary lives here, on the connection, not only on the URL the
	// registry returned. Checking the URL alone leaves two ways past it: a
	// redirect off the vetted host, and a name that resolves once for the check
	// and again for the dial.
	//
	// The transport is cloned rather than built fresh, to keep the proxy, timeout
	// and HTTP/2 defaults; only the dial is ours. The clone is wrapped so each
	// round trip records which proxy the transport selected for that request —
	// the dial hook needs it, and by dial time the request is out of sight.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if err := c.permitDial(ctx, address); err != nil {
			return nil, err
		}
		return dialer.DialContext(ctx, network, address)
	}
	c.HTTP = &http.Client{
		Timeout:       5 * time.Minute,
		Transport:     &proxyStamp{base: transport},
		CheckRedirect: c.checkRedirect,
	}
	return c
}

// proxyKey carries the proxy the transport selected for one request from the
// round trip, where the request is visible, to the dial, where it is not.
type proxyKey struct{}

// proxyStamp asks the transport which proxy it will use for a request and stamps
// the answer on the request's context before handing it over. permitDial reads
// it back, so the exemption it grants is for the proxy this request actually
// goes through — not for any proxy configured somewhere in the environment.
type proxyStamp struct{ base *http.Transport }

func (t *proxyStamp) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.base.Proxy != nil {
		u, err := t.base.Proxy(req)
		if err != nil {
			return nil, err
		}
		if u != nil {
			req = req.WithContext(context.WithValue(req.Context(), proxyKey{}, u))
		}
	}
	return t.base.RoundTrip(req)
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
	// Anonymous means no key was presented, so the upload belongs to nobody yet
	// and expires sooner.
	Anonymous bool `json:"anonymous,omitempty"`
	// ClaimURL adopts an anonymous upload into a workspace. Anyone holding it
	// can claim the upload, so it is a capability: it is printed for the person
	// who ran the push and deliberately kept out of the paste-ready output.
	ClaimURL string `json:"claim_url,omitempty"`
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
		return nil
	})
	if err != nil {
		return nil, err
	}
	// A 200 saying valid:false is still a rejection; surface it as one.
	if !key.Valid {
		return nil, Fail("invalid_key",
			"the registry does not recognise this key — run `krowk auth login --token krk_...` with a current one")
	}
	return &key, nil
}

// GetArtifact looks up a finalized artifact. The claim URL of an anonymous
// upload is deliberately not part of this response: the ID travels in the
// shareable link, so knowing it must not be enough to adopt the upload.
func (c *Client) GetArtifact(ctx context.Context, id string) (*Artifact, error) {
	if id == "" {
		return nil, Fail("missing_id", "pass the artifact ID, the last part of the link — e.g. 9f3c2e1")
	}
	endpoint, err := c.sameOrigin(c.BaseURL + "/artifacts/" + url.PathEscape(id))
	if err != nil {
		return nil, err
	}

	var artifact Artifact
	err = c.retry(ctx, func() error {
		req, err := c.request(ctx, http.MethodGet, endpoint, nil, "")
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
		return nil, malformed("the registry returned an artifact without a canonical URL")
	}
	return &artifact, nil
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
	endpoint, err := c.storageOrigin(target.URL)
	if err != nil {
		return err
	}
	// Always PUT, whatever target.Method says. The contract documents one method,
	// and letting a response body choose it would hand a compromised registry the
	// method, host, path, headers and body of a request this process issues from
	// its own network position — a CI runner can reach a great deal that the
	// registry cannot.
	method := http.MethodPut

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
		// checkRedirect and permitDial refuse with an *Error of their own, which
		// the client hands back wrapped. Why the request was refused is more use
		// to the caller than "cannot reach the registry".
		var refused *Error
		if errors.As(err, &refused) {
			return nil, refused
		}
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

// storageOrigin accepts the object-storage host a presigned URL names — that is
// the whole point of presigning — but only a host worth sending bytes to.
//
// A file:// or data: target would make the client read something local. An
// address inside the machine or its network is worse: it turns the client into a
// confused deputy, issuing requests from a position the registry could not reach
// on its own. Plaintext http is refused too, since the bytes are the artifact.
// Two origins are exempt because the user chose them: the local registry, and
// the API's own origin — a self-hosted registry serves blobs on its own host.
func (c *Client) storageOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", malformed("the registry returned an upload URL that is not http(s): " + raw)
	}

	// Talking to a local registry means local upload targets, by definition.
	if c.isLocal() {
		return u.String(), nil
	}
	// A registry that serves blobs on its own host — this repository's own does —
	// points the upload at the origin the user already configured. That host is
	// trusted on the API's own terms, scheme included, so a self-hosted registry
	// on a private network keeps working.
	if base, err := url.Parse(c.BaseURL); err == nil &&
		u.Scheme == base.Scheme && u.Host == base.Host {
		return u.String(), nil
	}
	if u.Scheme != "https" {
		return "", Fail("insecure_upload_url",
			"the registry asked for a plaintext upload to "+u.Host+" — refusing to send the artifact over http")
	}
	if internalHost(u.Hostname()) {
		return "", Fail("untrusted_endpoint",
			"the registry pointed the upload at "+u.Host+", which is inside this machine or its network — refusing")
	}
	return u.String(), nil
}

// checkRedirect refuses to follow a redirect away from an upload target.
//
// storageOrigin vets the URL the registry returned; it cannot vet where that URL
// points next. Following one hop hands back everything the vetting took away —
// the host, the path, and the method too, since a 302 arrives at the next host as
// a GET. A presigned URL is a final destination and does not redirect, so a
// redirect here means the registry is aiming this process somewhere, and the
// honest answer is to fail the upload rather than complete it against whatever
// answered. The API's own origin may still redirect (http -> https in front of a
// self-hosted registry is ordinary), and that allowance covers upload URLs on
// that origin too — a registry serving its own blobs may reshuffle its paths;
// the downgrade check and permitDial cover where such a hop can land.
func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if _, err := c.sameOrigin(via[0].URL.String()); err != nil {
		return Fail("upload_redirected",
			"the upload target "+via[0].URL.Host+" redirected to "+req.URL.Host+
				" — a presigned URL is where the bytes belong, so this is not followed")
	}
	// Go forwards Authorization to the same host on a redirect, and judges "same"
	// by host alone — so https -> http on one host keeps carrying the token, in
	// the clear. Refuse the downgrade rather than rely on nobody arranging it.
	if last := via[len(via)-1].URL; last.Scheme == "https" && req.URL.Scheme != "https" {
		return Fail("insecure_redirect",
			"the registry redirected from https to "+req.URL.Scheme+" — refusing, the token would go in the clear")
	}
	if len(via) >= 10 {
		return Fail("too_many_redirects",
			"gave up after 10 redirects from "+via[0].URL.Host+" — the registry is looping")
	}
	return nil
}

// permitDial is the boundary applied to the address actually being connected to,
// after the resolver has had its say.
//
// internalHost judges a name by resolving it, but the transport resolves again
// when it dials, and the registry owns the name in the case that matters — so a
// zero-TTL record can answer public for the check and internal for the dial.
// Deciding here instead leaves nothing between the check and the connection.
//
// With an HTTP proxy in play this sees the proxy's address rather than the
// target's; the URL-level checks still stand in that case.
func (c *Client) permitDial(ctx context.Context, address string) error {
	// A local registry means local addresses throughout — that is the point of it.
	if c.isLocal() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil
	}
	// A zone makes an address unparseable — "fe80::1%eth0" — and an unparseable
	// address would otherwise be waved through.
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	ip := net.ParseIP(host)
	if ip == nil || !reservedIP(ip) {
		return nil
	}
	// The host the user typed into KROWK_API_URL is trusted the way isLocal
	// already trusts loopback: a self-hosted registry on a private network is
	// configuration, not the registry steering this process somewhere.
	if c.isAPIAddress(address) {
		return nil
	}
	// One other reserved address is legitimate: the proxy this request goes
	// through. Corporate proxies sit on private addresses, so refusing this
	// outright would refuse every upload from the CI environments this tool
	// exists for. Only the proxy the transport selected for this request earns
	// the exemption though — a request that skipped the proxy, because NO_PROXY
	// covers its host or its scheme names a different variable, is judged like
	// any other, even when its target resolves to a configured proxy's address.
	if proxy, ok := ctx.Value(proxyKey{}).(*url.URL); ok &&
		slices.Contains(hostAddresses(proxy.Hostname(), proxy.Port(), proxy.Scheme), address) {
		return nil
	}
	return Fail("untrusted_endpoint",
		"refusing to connect to "+address+", which is inside this machine or its network")
}

// isAPIAddress reports whether address is where BaseURL's own host lives.
//
// Resolved here rather than cached at startup, and only on the path that is about
// to refuse a connection: a registry that moved to a new address would otherwise
// break every upload until the process restarted, which is a bad trade for
// saving a lookup on a path taken a handful of times per run.
func (c *Client) isAPIAddress(address string) bool {
	base, err := url.Parse(c.BaseURL)
	if err != nil || base.Hostname() == "" {
		return false
	}
	return slices.Contains(hostAddresses(base.Hostname(), base.Port(), base.Scheme), address)
}

// hostAddresses turns a host into the addresses a dial to it would use — the
// literal host:port, and the resolved ones, since a dial happens after
// resolution. An empty port falls back to the scheme's default, the way a URL
// without one is dialled.
func hostAddresses(hostname, port, scheme string) []string {
	if port == "" {
		switch scheme {
		case "https":
			port = "443"
		case "socks5", "socks5h":
			port = "1080"
		default:
			port = "80"
		}
	}
	out := []string{net.JoinHostPort(hostname, port)}
	if ips, err := net.LookupIP(hostname); err == nil {
		for _, ip := range ips {
			out = append(out, net.JoinHostPort(ip.String(), port))
		}
	}
	return out
}

// isLocal reports whether this client is pointed at a registry on this machine,
// where loopback upload targets are expected rather than suspicious.
func (c *Client) isLocal() bool {
	base, err := url.Parse(c.BaseURL)
	if err != nil {
		return false
	}
	return isLoopback(base.Hostname())
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// internalHost reports whether a host names something only reachable from where
// this process happens to be sitting. A literal IP is judged directly; a name is
// resolved, because "metadata.internal" is a name.
func internalHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return reservedIP(ip)
	}
	// A name that will not resolve is not obviously internal; let the request
	// fail on its own rather than guessing. A name that resolves to anything
	// reserved is refused, so a single bad answer is enough to stop it.
	addrs, err := net.LookupIP(host)
	if err != nil {
		return false
	}
	for _, ip := range addrs {
		if reservedIP(ip) {
			return true
		}
	}
	return false
}

// What Go's own predicates leave out. Each of these reaches somewhere an
// artifact upload has no business going.
var reservedRanges = []net.IPNet{
	// Carrier-grade NAT: not private by Go's reckoning, and where Tailscale and
	// several CI providers put their internal hosts.
	{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)},
	{IP: net.IPv4(198, 18, 0, 0), Mask: net.CIDRMask(15, 32)},
	{IP: net.IPv4(192, 0, 0, 0), Mask: net.CIDRMask(24, 32)},
	// 240.0.0.0/4, reserved, and it carries the broadcast address with it.
	{IP: net.IPv4(240, 0, 0, 0), Mask: net.CIDRMask(4, 32)},
	// NAT64. On an IPv6-only network 64:ff9b::a9fe:a9fe is the metadata service,
	// and it is neither loopback, private nor link-local to Go — this is the
	// standard way past a check that only knows the IPv4 shapes.
	{IP: net.ParseIP("64:ff9b::"), Mask: net.CIDRMask(96, 128)},
}

func reservedIP(ip net.IP) bool {
	if ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast() ||
		ip.IsInterfaceLocalMulticast() {
		return true
	}
	return slices.ContainsFunc(reservedRanges, func(n net.IPNet) bool { return n.Contains(ip) })
}

func malformed(detail string) *Error {
	return &Error{Body: map[string]any{
		"error":     "malformed_response",
		"detail":    detail,
		"fix":       "the registry is not speaking the upload handshake this client expects",
		"retryable": false,
	}}
}
