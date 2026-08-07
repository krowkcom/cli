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
// artifact, the claim on one, the run it belongs to, the completion of a run. The
// verb follows from whether the call can be repeated: finalizing, completing and
// setting an artifact's run are idempotent, so they are PUTs; claiming spends a
// one-shot token, so it is a POST. Checking a key is the same rule read the other
// way — GET /v1/key names the key this request is made with, because asking what
// it may do changes nothing about it.
package api

import (
	"bytes"
	"context"
	"crypto/rand"
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
	"syscall"
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
	// ControlContext rather than DialContext: the transport hands DialContext the
	// URL's host:port before resolution, so a judgement there never sees where a
	// name actually lands. ControlContext runs inside the dialer, per resolved
	// candidate address, and still carries the request context the proxy
	// exemption needs.
	transport.DialContext = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		ControlContext: func(ctx context.Context, network, address string, _ syscall.RawConn) error {
			return c.permitDial(ctx, address)
		},
	}).DialContext
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

// Key is the key a request is made with, as the registry reports it. Asking
// beats guessing: a token string says nothing about itself — it can be revoked,
// expired, or for a workspace other than the one the caller expects, and all
// three look identical until an upload fails.
type Key struct {
	KeyID      string `json:"key_id,omitempty"`
	Name       string `json:"name,omitempty"`
	Workspace  string `json:"workspace,omitempty"`
	ExpiresAt  string `json:"expires_at,omitempty"`
	LastUsedAt string `json:"last_used_at,omitempty"`
	CreatedAt  string `json:"created_at,omitempty"`
	// Status is the HTTP status the read answered with. Diagnostics print what
	// actually arrived rather than assuming 200. Transport detail, not part of
	// the key itself.
	Status int `json:"-"`
}

// VerifyKey reads back the key the client is holding. There is no "valid" field
// to consult: a key the registry will not accept gets the same 401 here as
// anywhere else, so a success is the answer. What is checked is that a key came
// back at all — a 200 with no key_id is some other service answering, not a
// registry saying yes.
func (c *Client) VerifyKey(ctx context.Context) (*Key, error) {
	var key Key
	var status int
	if err := c.callStatus(ctx, http.MethodGet, "/key", nil, &key, &status); err != nil {
		return nil, err
	}
	key.Status = status
	if key.KeyID == "" {
		return nil, &Error{Status: status, Body: map[string]any{
			"error":     "malformed_response",
			"fix":       "the registry answered the key lookup without naming a key — check KROWK_API_URL points at the API host, not the website",
			"retryable": false,
		}}
	}
	return &key, nil
}

// KeyRejected reports whether an error from VerifyKey is the registry saying it
// will not accept this key, as opposed to not managing to answer at all.
//
// The distinction is the whole of `auth login`'s judgement. A 401 is a verdict
// on the key itself and there is nothing to retry — the token is wrong now and
// will be wrong later. Anything else, from no network to a 503 to something
// that is not a registry answering, is a verdict on the moment, and the key may
// well be fine. Only the first is grounds for refusing to store it.
func KeyRejected(err error) bool {
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden
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

// slugPath escapes a slug into a URL path segment.
//
// A real slug is letters and digits, so this changes nothing for one. It is here
// for everything else a caller can type: `#` ends the path and makes the rest a
// fragment, `?` starts a query, and `/` invents a segment — so an unescaped slug
// does not fail, it silently addresses a *different* endpoint. On a takedown
// that means destroying an artifact other than the one named, and on a read it
// means an answer about something the caller never asked for.
func slugPath(slug string) string { return url.PathEscape(slug) }

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
//
// Named with an Idempotency-Key, because this is the call a lost response makes
// expensive: without one a retry declares a second artifact, the first is left to
// expire, and both count as uploads. With one, the retry is answered with the
// artifact the first attempt already made.
func (c *Client) PrepareArtifact(ctx context.Context, spec Spec) (*Artifact, error) {
	var artifact Artifact
	body := map[string]any{"artifact": spec}
	if err := c.callIdempotent(ctx, http.MethodPost, "/artifacts", body, &artifact); err != nil {
		return nil, err
	}
	return &artifact, nil
}

// FinalizeArtifact confirms the upload landed. A PUT because it is idempotent:
// the artifact ends up in the same state however many times it is asked for, so a
// retry is a success rather than an error.
func (c *Client) FinalizeArtifact(ctx context.Context, slug string) (*Artifact, error) {
	var artifact Artifact
	if err := c.call(ctx, http.MethodPut, "/artifacts/"+slugPath(slug)+"/finalization", nil, &artifact); err != nil {
		return nil, err
	}
	return &artifact, nil
}

// ShowArtifact reads one artifact back. Works without a key: for a keyless
// request the slug is the capability, and it resolves within the anonymous
// workspace.
func (c *Client) ShowArtifact(ctx context.Context, slug string) (*Artifact, error) {
	var artifact Artifact
	if err := c.call(ctx, http.MethodGet, "/artifacts/"+slugPath(slug), nil, &artifact); err != nil {
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
	var page Page
	if err := c.call(ctx, http.MethodGet, paged("/artifacts", before, limit), nil, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// paged adds the cursor and page size every listing takes. One spelling for all
// of them, because the registry decides both in one place too — a client that
// paged one listing differently from another would only ever do so by accident.
func paged(path, before string, limit int) string {
	query := url.Values{}
	if before != "" {
		query.Set("before", before)
	}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	if len(query) == 0 {
		return path
	}
	return path + "?" + query.Encode()
}

// RunPage is one page of a workspace's runs, newest first. Next carries the slug
// to pass back as before for the following page, and is empty on the last.
type RunPage struct {
	Runs []*Run `json:"runs"`
	Next string `json:"next,omitempty"`
}

// ListRuns reads a page of the key's runs. Needs a key: a run belongs to a
// workspace, and a keyless caller has none of its own.
func (c *Client) ListRuns(ctx context.Context, before string, limit int) (*RunPage, error) {
	var page RunPage
	if err := c.call(ctx, http.MethodGet, paged("/runs", before, limit), nil, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// ShowRun reads one run back — its status, when it started and finished, and the
// metadata recorded on it, which is where everything about an upload's origin
// lives since the registry keeps none on the artifact itself.
func (c *Client) ShowRun(ctx context.Context, slug string) (*Run, error) {
	var run Run
	if err := c.call(ctx, http.MethodGet, "/runs/"+slugPath(slug), nil, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// ListRunArtifacts reads a page of what one run produced.
//
// A collection of the run rather than a filter on the workspace listing, which
// is the registry's shape and worth keeping: the run is looked up first, so an
// unknown slug is a 404. A filter would answer an empty page instead, and a
// caller cannot tell that apart from a run that genuinely produced nothing.
func (c *Client) ListRunArtifacts(ctx context.Context, runSlug, before string, limit int) (*Page, error) {
	var page Page
	path := paged("/runs/"+slugPath(runSlug)+"/artifacts", before, limit)
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
	if err := c.call(ctx, http.MethodPost, "/artifacts/"+slugPath(slug)+"/claim", body, &artifact); err != nil {
		return nil, err
	}
	return &artifact, nil
}

// AttachRun puts an artifact under a run after it was uploaded, which is the
// only way an upload that started out anonymous ever gets one: a keyless upload
// cannot name a run at create time, and claiming it does not give it one.
//
// A PUT for the same reason finalizing is one: the artifact ends up under the
// same run however many times it is asked for, so a retry is a success rather
// than an error. An artifact belongs to one run, so naming a different one moves
// it. Both slugs resolve in the key's workspace, so an artifact has to be
// claimed before it can be attached.
func (c *Client) AttachRun(ctx context.Context, artifactSlug, runSlug string) (*Artifact, error) {
	var artifact Artifact
	body := map[string]any{"run": runSlug}
	if err := c.call(ctx, http.MethodPut, "/artifacts/"+slugPath(artifactSlug)+"/run", body, &artifact); err != nil {
		return nil, err
	}
	return &artifact, nil
}

// TakeDownArtifact removes an artifact's bytes and leaves a tombstone behind, so
// the link answers 410 rather than 404 — it is already pasted somewhere, and its
// reader deserves to be told the artifact was removed rather than sent hunting
// for a typo.
//
// Immediate and unrecoverable, which is the point rather than a limitation. This
// is what someone reaches for when a secret was uploaded by accident, and a
// secret that can be restored is still leaked — so nothing here routes through a
// window that could hand the bytes back.
//
// Two authorities take an artifact down, and which one is used decides how the
// request is made. A key's authority is the workspace it acts in. A claim
// token's is the one artifact it was issued for, and it is needed at all because
// a slug travels in whatever the link was pasted into: authorising a takedown by
// slug alone would let every reader of the paste destroy what they read.
//
// Nothing comes back but a 204. There is no artifact left to report, and a url
// and markdown naming bytes that are gone would be a lie.
//
// Retried like any other call: taking down what is already down is a success on
// both sides, so a lost response costs nothing to ask again.
func (c *Client) TakeDownArtifact(ctx context.Context, slug, claimToken string) error {
	if claimToken == "" {
		return c.call(ctx, http.MethodDelete, "/artifacts/"+slugPath(slug), nil, nil)
	}

	// A claim token is the authority for this call, and it is the only one the
	// registry will read: offered alongside a key, the key wins outright and the
	// lookup happens in that key's workspace — where an artifact still sitting in
	// the anonymous one is simply not found. So the key is withheld rather than
	// sent and ignored, which is what makes taking down a CI job's anonymous
	// upload work from a machine that happens to be logged in.
	//
	// The token goes in the body rather than the query string for the same reason
	// a claim's does: a query string ends up in access logs, and this token is a
	// capability.
	keyless := New(c.BaseURL, "")
	keyless.Sleep = c.Sleep
	body := map[string]any{"claim_token": claimToken}
	return keyless.call(ctx, http.MethodDelete, "/artifacts/"+slugPath(slug), body, nil)
}

// CreateRun opens a run to hang artifacts off. Needs a key: a run belongs to a
// workspace, and a keyless upload has none.
//
// Retried, now that an Idempotency-Key makes it safe to. It used to be sent
// once and only once: a run committed under a lost response would be duplicated
// by the retry, an orphan whose slug never surfaces anywhere, and that was worse
// than failing the push outright. The key removes the choice — the retry is
// answered with the run the first attempt opened — so a transient 502 on the way
// to opening a run no longer costs the whole upload.
func (c *Client) CreateRun(ctx context.Context, metadata any) (*Run, error) {
	var run Run
	body := map[string]any{"run": map[string]any{"metadata": metadata}}
	if err := c.callIdempotent(ctx, http.MethodPost, "/runs", body, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// FinishRun closes a run. A PUT for the same reason finalizing is one: a CI
// cleanup step that runs twice should get the same success both times, and the
// run keeps the moment it first finished.
func (c *Client) FinishRun(ctx context.Context, slug string) (*Run, error) {
	var run Run
	if err := c.call(ctx, http.MethodPut, "/runs/"+slugPath(slug)+"/completion", nil, &run); err != nil {
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
	endpoint, err := c.storageOrigin(up.URL)
	if err != nil {
		return err
	}
	// Always PUT, whatever up.Method says. The contract documents one method,
	// and letting a response body choose it would hand a compromised registry the
	// method, host, path, headers and body of a request this process issues from
	// its own network position — a CI runner can reach a great deal that the
	// registry cannot.
	method := http.MethodPut

	var last error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return Fail("cancelled", err.Error())
		}
		// The body is a file handle, so it has to be reopened for every attempt.
		f, err := os.Open(spec.Path)
		if err != nil {
			return Fail("file_unreadable", "cannot read `"+spec.Path+"`: "+err.Error())
		}

		req, err := http.NewRequestWithContext(ctx, method, endpoint, f)
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
		// checkRedirect and permitDial refuse with an *Error of their own, which
		// the client hands back wrapped. Why the request was refused is more use
		// to the caller than "cannot reach storage".
		var refused *Error
		if errors.As(err, &refused) {
			return refused
		}
		return &Error{Body: map[string]any{
			"error":     "storage_unreachable",
			"detail":    err.Error(),
			"fix":       "the registry issued an upload URL but the bytes could not be sent to it — check the network",
			"retryable": true,
		}}
	}
	defer res.Body.Close()

	// checkRedirect hands 3xx responses back instead of following them; treat
	// them as the refusal they are rather than as a success.
	if res.StatusCode >= 300 && res.StatusCode < 400 {
		return &Error{Status: res.StatusCode, Body: map[string]any{
			"error":     "unexpected_redirect",
			"endpoint":  req.URL.String(),
			"location":  res.Header.Get("Location"),
			"fix":       "the server redirected this request — following it could carry the request past the origin checks, so it is not followed",
			"retryable": false,
		}}
	}

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
	return c.doAttempts(ctx, method, c.BaseURL+path, body, out, maxAttempts, nil, "")
}

// callStatus is call for the rare caller that needs the HTTP status a success
// arrived with, not just the decoded body.
func (c *Client) callStatus(ctx context.Context, method, path string, body any, out any, status *int) error {
	return c.doAttempts(ctx, method, c.BaseURL+path, body, out, maxAttempts, status, "")
}

// callOnce is call without the retry loop, for requests that must not be
// repeated on a lost response.
func (c *Client) callOnce(ctx context.Context, method, path string, body any, out any) error {
	return c.doAttempts(ctx, method, c.BaseURL+path, body, out, 1, nil, "")
}

// callIdempotent is call for the two endpoints that create something. One key is
// minted here and carried by every attempt of this one call, which is what makes
// the retry loop safe on a POST: the registry answers a key it has already seen
// with the record it made the first time, so a lost response costs a duplicate
// artifact and a duplicate upload charge no longer.
//
// Deliberately per call rather than per `push`. A push declares one artifact per
// file, and the registry matches a key against the payload it was first used with
// — so one key spread across three files would have the second file refused as a
// reuse. It does span the run and the artifact of the same call, since the
// registry scopes a key by what it creates.
//
// A fresh `krowk push` mints fresh keys, and that is right: re-running the
// command is a new attempt, not a retry of the old one.
func (c *Client) callIdempotent(ctx context.Context, method, path string, body any, out any) error {
	key, err := newIdempotencyKey()
	if err != nil {
		return err
	}
	return c.doAttempts(ctx, method, c.BaseURL+path, body, out, maxAttempts, nil, key)
}

func (c *Client) do(ctx context.Context, method, url string, body any, out any) error {
	return c.doAttempts(ctx, method, url, body, out, maxAttempts, nil, "")
}

// newIdempotencyKey is 128 random bits, shaped as a version 4 UUID because that
// is the shape the registry's documentation asks for.
//
// Unguessable rather than merely unique, and that part is load-bearing on a
// keyless push. With no API key there is nothing else a retry can present to
// prove it is the client that made the original call, so the registry scopes
// keys by address — and a predictable key would let anyone sharing that address,
// a NAT or a CI runner pool, replay this declare and be handed a URL to write
// its bytes.
func newIdempotencyKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", Fail("no_idempotency_key",
			"this machine's random source is unreadable, so a retry could not be named safely")
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func (c *Client) doAttempts(ctx context.Context, method, url string, body any, out any, attempts int, status *int, idempotencyKey string) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return Fail("bad_request", "the request body could not be encoded as JSON")
		}
	}

	var last error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return Fail("cancelled", err.Error())
		}
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
		// Set inside the loop but the same value every time, which is the whole
		// point: it is what tells the registry that attempt two is attempt one
		// again rather than a second create.
		if idempotencyKey != "" {
			req.Header.Set("Idempotency-Key", idempotencyKey)
		}

		err = c.doOnce(req, out, status)
		if err == nil {
			return nil
		}
		last = err

		var apiErr *Error
		if !errors.As(err, &apiErr) || !apiErr.Retryable() || attempt == attempts {
			return err
		}
		c.Sleep(backoff(apiErr, attempt))
	}
	return last
}

func (c *Client) doOnce(req *http.Request, out any, status *int) error {
	res, err := c.HTTP.Do(req)
	if err != nil {
		// checkRedirect and permitDial refuse with an *Error of their own, which
		// the client hands back wrapped. Why the request was refused is more use
		// to the caller than "cannot reach the registry".
		var refused *Error
		if errors.As(err, &refused) {
			return refused
		}
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
	if status != nil {
		*status = res.StatusCode
	}

	// checkRedirect hands 3xx responses back instead of following them; treat
	// them as the refusal they are rather than as a body to decode.
	if res.StatusCode >= 300 && res.StatusCode < 400 {
		return &Error{Status: res.StatusCode, Body: map[string]any{
			"error":     "unexpected_redirect",
			"endpoint":  req.URL.String(),
			"location":  res.Header.Get("Location"),
			"fix":       "the server redirected this request — following it could carry the request past the origin checks, so it is not followed",
			"retryable": false,
		}}
	}

	if res.StatusCode >= 400 {
		err := responseError(res, payload, readErr)
		// A 401 with no key sent is a missing key, not a rejected one — and
		// "check your token" is unhelpful advice when there is no token to check.
		if err.Code() == "unauthorized" {
			if c.Token == "" {
				err.Body["fix"] = "this endpoint needs an API key — run `krowk auth login --token krowk_sk_...`, or set KROWK_TOKEN"
			} else if !strings.HasSuffix(req.URL.Path, "/key") {
				// A key was sent and rejected, which is exactly the moment the
				// self-check earns its keep — the registry cannot know the CLI has
				// a verify command, so the client adds that half of the fix itself.
				// Except on the self-check: telling `auth verify` to run
				// `auth verify` is a loop, not a fix.
				hint := "run `krowk auth verify` to see what this key is allowed to do"
				if fix, _ := err.Body["fix"].(string); fix != "" {
					err.Body["fix"] = fix + " — " + hint
				} else {
					err.Body["fix"] = hint
				}
			}
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
		// Only an upload naming a run reaches this: the registry raises it where a
		// request may legitimately arrive keyless, and refuses the attach route with a
		// plain 401 instead. So naming --run here is right, not push-specific.
		return "attaching an upload to a run needs an API key — authenticate, or upload without --run"
	case "upload_missing":
		return "the bytes had not landed when the upload was finalized — retry the upload"
	case "checksum_mismatch":
		return "the file changed while it was being uploaded — retry the upload"
	case "empty_upload":
		return "what arrived held no bytes — check the file is not being written while it is uploaded"
	case "expired":
		return "this artifact was anonymous and has passed its expiry — upload it again, and claim it with a key to keep it"
	case "taken_down":
		// 410 rather than 404 because the link is already pasted somewhere, so the
		// fix says the artifact existed and is gone rather than sending someone
		// hunting for a typo. A takedown is unrecoverable by design; uploading
		// again is the only way forward, and it is a new slug and a new link.
		return "this artifact was taken down and its bytes are gone for good — upload it again if the link is still needed"
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
		"taken_down", "checksum_mismatch", "empty_upload", "run_needs_key":
		return false, true
	}
	return false, false
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

	if secs, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
		if secs <= 0 {
			return 0, false
		}
		// Capped before the multiply: a huge integer would overflow
		// time.Duration and wrap negative, sailing straight past min.
		if secs > int64(maxWait/time.Second) {
			return maxWait, true
		}
		return time.Duration(secs) * time.Second, true
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

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// isAPIOrigin reports whether u shares the API base's scheme and host — the
// line between failures where "check KROWK_API_URL" is the fix and failures
// on a storage host where it would only mislead.
func (c *Client) isAPIOrigin(u *url.URL) bool {
	base, err := url.Parse(c.BaseURL)
	return err == nil && base.Scheme == u.Scheme && base.Host == u.Host
}

// onAPIOrigin reports whether u is on the origin the user configured, counting
// the https upgrade of an http base — same host, default https port, upgrade
// direction only. It is the one judgement of "the API's own origin" shared by
// the upload-URL check, the redirect gate and (via isAPIAddress) the dial hook,
// so a URL one layer permits is not refused by the next.
func (c *Client) onAPIOrigin(u *url.URL) bool {
	base, err := url.Parse(c.BaseURL)
	if err != nil || base.Hostname() == "" || u.Hostname() != base.Hostname() {
		return false
	}
	if u.Scheme == base.Scheme && defaultedPort(u) == defaultedPort(base) {
		return true
	}
	return base.Scheme == "http" && u.Scheme == "https" && defaultedPort(u) == "443"
}

// defaultedPort is the port a dial to u would use: the explicit one, or the
// scheme's default — so https://host and https://host:443 name one origin, the
// way the dial layer already treats them.
func defaultedPort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
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
		return "", Fail("bad_upload_url",
			"the registry returned an upload URL that is not http(s): "+raw)
	}

	// Talking to a local registry means local upload targets, by definition.
	if c.isLocal() {
		return u.String(), nil
	}
	// A registry that serves blobs on its own host — this repository's own
	// stand-in does — points the upload at the origin the user already
	// configured. That host is trusted on the API's own terms, so a self-hosted
	// registry on a private network keeps working, https upgrade included.
	if c.onAPIOrigin(u) {
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
// that origin too — a registry serving its own blobs may reshuffle its paths —
// but every hop stays on that host; the allowance is the origin the user
// configured, not a first hop that launders the rest.
func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if !c.onAPIOrigin(via[0].URL) {
		return Fail("upload_redirected",
			"the upload target "+via[0].URL.Host+" redirected to "+req.URL.Host+
				" — a presigned URL is where the bytes belong, so this is not followed")
	}
	// The request started on the API's own origin, so it stays on that host, hop
	// after hop. The scheme may change — http -> https in front of a self-hosted
	// registry is the ordinary case — and the downgrade below is refused anyway.
	// onAPIOrigin above already proved BaseURL parses with a host.
	if base, _ := url.Parse(c.BaseURL); req.URL.Hostname() != base.Hostname() {
		return Fail("untrusted_redirect",
			"the registry redirected a request from its own origin to "+req.URL.Host+
				" — a request to the API's origin stays there")
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
	// Same hostname is not the same origin: Go forwards Authorization to any port
	// on the host, so a hop to another port would deliver the token to a
	// different server. onAPIOrigin counts the https upgrade and nothing wider;
	// anything else hands the 3xx back and the caller reports it as
	// unexpected_redirect.
	if !c.onAPIOrigin(req.URL) {
		return http.ErrUseLastResponse
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
	if slices.Contains(hostAddresses(base.Hostname(), base.Port(), base.Scheme), address) {
		return true
	}
	// checkRedirect permits an API-origin request to upgrade its scheme —
	// http -> https in front of a self-hosted registry is the ordinary case —
	// and the upgraded hop dials port 443, not the base's. Cover exactly that
	// hop: the same host's addresses, one extra port, upgrade direction only.
	return base.Scheme == "http" &&
		slices.Contains(hostAddresses(base.Hostname(), "443", "https"), address)
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
