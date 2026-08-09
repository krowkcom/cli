package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krowkcom/cli/internal/output"
	"github.com/krowkcom/cli/internal/registry"
)

// isolateConfig gives a test an empty config directory of its own.
//
// The credentials path is read from the process environment rather than from
// the harness's, so without this every test sees whatever key the person
// running the suite happens to be logged in with. A test that drops KROWK_TOKEN
// to exercise the anonymous path then quietly gets their key instead, and the
// suite passes or fails depending on whose machine it is on.
func isolateConfig(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

// harness runs the CLI against a throwaway registry, the way a user would.
type harness struct {
	t       *testing.T
	env     map[string]string
	server  *httptest.Server
	fixture string
}

func newHarness(t *testing.T, limitBytes int64) *harness {
	t.Helper()

	server := httptest.NewServer(registry.Handler(limitBytes, ""))
	t.Cleanup(server.Close)

	isolateConfig(t)

	h := &harness{
		t:      t,
		server: server,
		env: map[string]string{
			"KROWK_API_URL": server.URL + "/v1",
			"KROWK_TOKEN":   "krowk_sk_test",
		},
	}
	h.fixture = h.write("checkout-after.png", "fake png bytes for the test")
	return h
}

// anonymous drops the key, which is what a first-run agent looks like.
func (h *harness) anonymous() *harness {
	delete(h.env, "KROWK_TOKEN")
	return h
}

func (h *harness) write(name, contents string) string {
	h.t.Helper()
	path := filepath.Join(h.t.TempDir(), name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		h.t.Fatal(err)
	}
	return path
}

func (h *harness) run(args ...string) (code int, stdout, stderr string) {
	h.t.Helper()
	var out, errOut bytes.Buffer
	code = Run(args, &out, &errOut, func(k string) string { return h.env[k] }, false)
	return code, out.String(), errOut.String()
}

// ok runs a command that is expected to succeed and decodes its envelope.
func (h *harness) ok(args ...string) envelope {
	h.t.Helper()
	code, stdout, stderr := h.run(args...)
	if code != 0 {
		h.t.Fatalf("`krowk %s` exited %d, stderr:\n%s", strings.Join(args, " "), code, stderr)
	}
	return decode(h.t, stdout)
}

// fails runs a command that is expected to fail and returns its error body.
//
// It checks only that the exit was non-zero, because which non-zero number a
// failure earns is the exit-code taxonomy's business: exitcode_test.go pins
// every case, and failsWith below is how a test says the number is the point.
// Asserting 1 here instead would make every classified failure look like a
// regression in the test that happened to provoke it.
func (h *harness) fails(args ...string) map[string]any {
	h.t.Helper()
	code, stdout, stderr := h.run(args...)
	if code == 0 {
		h.t.Fatalf("`krowk %s` succeeded, want a failure, stdout:\n%s", strings.Join(args, " "), stdout)
	}
	return decode(h.t, stderr).Error
}

// failsWith is fails for a test whose subject is the exit code itself.
func (h *harness) failsWith(want int, args ...string) map[string]any {
	h.t.Helper()
	code, stdout, stderr := h.run(args...)
	if code != want {
		h.t.Fatalf("`krowk %s` exited %d, want %d, stdout:\n%s",
			strings.Join(args, " "), code, want, stdout)
	}
	return decode(h.t, stderr).Error
}

// get fetches a URL the registry handed out, which is how a test proves the
// bytes really travelled rather than only that the calls returned 200.
func (h *harness) get(url string) (int, string) {
	h.t.Helper()
	res, err := http.Get(url)
	if err != nil {
		h.t.Fatalf("GET %s: %v", url, err)
	}
	defer res.Body.Close()
	var body bytes.Buffer
	if _, err := body.ReadFrom(res.Body); err != nil {
		h.t.Fatal(err)
	}
	return res.StatusCode, body.String()
}

// envelope is the JSON shape every non-human result comes back in.
type envelope struct {
	OK      bool   `json:"ok"`
	Data    data   `json:"data"`
	Summary string `json:"summary"`
	// The output package's own type, so a field added there is one these tests
	// see rather than one they silently drop.
	Breadcrumbs []output.Breadcrumb `json:"breadcrumbs"`
	Error       map[string]any      `json:"error"`
}

// data covers both shapes a command returns: an upload result, and a bare run.
type data struct {
	Artifacts []artifact     `json:"artifacts"`
	Runs      []run          `json:"runs"`
	Run       *run           `json:"run"`
	Notes     []string       `json:"notes"`
	Next      string         `json:"next"`
	Slug      string         `json:"slug"`
	Status    string         `json:"status"`
	Metadata  map[string]any `json:"metadata"`
	TakenDown bool           `json:"taken_down"`
}

type artifact struct {
	Slug        string `json:"slug"`
	State       string `json:"state"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	ByteSize    int64  `json:"byte_size"`
	Checksum    string `json:"checksum"`
	// Run is the run this artifact belongs to, nested rather than a bare slug:
	// the registry reports the run itself, metadata and all, so reading an
	// artifact back says what produced it without a second call.
	Run *run `json:"run"`
	// URL is the card page and FileURL is the object. Two fields because they
	// are two different links, and confusing them is the failure this whole
	// change exists to make impossible.
	URL        string `json:"url"`
	FileURL    string `json:"file_url"`
	Markdown   string `json:"markdown"`
	ExpiresAt  string `json:"expires_at"`
	ClaimToken string `json:"claim_token"`
}

// runSlug is "" when the artifact belongs to no run, which is what a keyless
// upload always is.
func (a artifact) runSlug() string {
	if a.Run == nil {
		return ""
	}
	return a.Run.Slug
}

type run struct {
	Slug     string         `json:"slug"`
	Status   string         `json:"status"`
	Metadata map[string]any `json:"metadata"`
}

func decode(t *testing.T, s string) envelope {
	t.Helper()
	var e envelope
	if err := json.Unmarshal([]byte(s), &e); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, s)
	}
	return e
}

// only returns the single artifact a result is expected to hold.
func only(t *testing.T, e envelope) artifact {
	t.Helper()
	if len(e.Data.Artifacts) != 1 {
		t.Fatalf("want exactly one artifact, got %d", len(e.Data.Artifacts))
	}
	return e.Data.Artifacts[0]
}

func TestPushUploadsTheBytesAndFinalizes(t *testing.T) {
	h := newHarness(t, 0)

	e := h.ok("push", h.fixture)
	if !e.OK {
		t.Fatalf("not ok: %+v", e)
	}

	a := only(t, e)
	// ready is the whole point: it means the registry looked at what landed in
	// storage and agreed it was what the client declared.
	if a.State != "ready" {
		t.Errorf("state = %q, want ready — the upload was never finalized", a.State)
	}
	if a.Filename != "checkout-after.png" || a.ContentType != "image/png" {
		t.Errorf("filename/type = %q/%q", a.Filename, a.ContentType)
	}
	if a.ByteSize != int64(len("fake png bytes for the test")) {
		t.Errorf("byte_size = %d", a.ByteSize)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(a.Checksum) {
		t.Errorf("checksum = %q, want a sha-256 the registry verified", a.Checksum)
	}

	// The link is the product. If it does not serve the bytes, nothing else here
	// matters.
	status, body := h.get(a.FileURL)
	if status != 200 || body != "fake png bytes for the test" {
		t.Errorf("GET %s = %d %q", a.FileURL, status, body)
	}
}

func TestMetadataIsRecordedOnTheRun(t *testing.T) {
	h := newHarness(t, 0)

	e := h.ok(
		"push", h.fixture,
		"--pull-request=https://github.com/acme/storefront/pull/412",
		"--reference=https://linear.app/acme/issue/ENG-9",
		"--reference=https://sentry.io/issues/1",
		"--session=sess_abc123",
		"--title=Checkout — mobile",
	)

	if e.Data.Run == nil {
		t.Fatal("no run: an authenticated upload should open one to hold the metadata")
	}
	// The run is closed on the way out, because this command opened it.
	if e.Data.Run.Status != "finished" {
		t.Errorf("run status = %q, want finished", e.Data.Run.Status)
	}
	if got := only(t, e).runSlug(); got != e.Data.Run.Slug {
		t.Errorf("artifact run = %q, want %q", got, e.Data.Run.Slug)
	}

	meta := e.Data.Run.Metadata
	if got := meta["pull_request"]; got != "https://github.com/acme/storefront/pull/412" {
		t.Errorf("pull_request = %v", got)
	}
	if got := meta["session"]; got != "sess_abc123" {
		t.Errorf("session = %v", got)
	}
	if got := meta["client"]; got != "krowk-cli/"+Version {
		t.Errorf("client = %v", got)
	}
	// Plural, and a list: --reference repeats, and canon names the run's field
	// `references`. The registry stores metadata verbatim, so this key is the
	// contract every reader of a run pins against.
	refs, _ := meta["references"].([]any)
	if len(refs) != 2 || refs[1] != "https://sentry.io/issues/1" {
		t.Errorf("references = %v, want both links in order", meta["references"])
	}
	// Detected without a flag: the CLI's own repository.
	if commit, _ := meta["commit"].(string); !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(commit) {
		t.Errorf("commit = %q, want a detected SHA", commit)
	}
}

func TestEveryFileGetsItsOwnLinkUnderOneRun(t *testing.T) {
	h := newHarness(t, 0)
	second := h.write("log.txt", "the build log")

	e := h.ok("push", h.fixture, second)

	if len(e.Data.Artifacts) != 2 {
		t.Fatalf("want 2 artifacts, got %d", len(e.Data.Artifacts))
	}
	first, last := e.Data.Artifacts[0], e.Data.Artifacts[1]
	if first.URL == last.URL {
		t.Errorf("both files share a link: %q", first.URL)
	}
	if e.Data.Run == nil || first.runSlug() != last.runSlug() || first.runSlug() != e.Data.Run.Slug {
		t.Errorf("artifacts are not grouped under one run: %q and %q", first.runSlug(), last.runSlug())
	}
	if last.ContentType != "text/plain" {
		t.Errorf("log.txt content type = %q", last.ContentType)
	}
	if _, body := h.get(last.FileURL); body != "the build log" {
		t.Errorf("second file served %q", body)
	}
}

func TestAnonymousUploadWorksAndSaysWhatItDropped(t *testing.T) {
	h := newHarness(t, 0).anonymous()

	e := h.ok("push", h.fixture, "--pull-request=https://github.com/acme/storefront/pull/412")

	a := only(t, e)
	if a.State != "ready" {
		t.Errorf("state = %q — a keyless upload should still complete", a.State)
	}
	if e.Data.Run != nil {
		t.Errorf("run = %+v, want none: a keyless upload has no workspace to open one in", e.Data.Run)
	}
	// Dropping the pull request silently would leave an agent believing it was
	// attached, so the result has to say otherwise.
	if len(e.Data.Notes) == 0 || !strings.Contains(e.Data.Notes[0], "--pull-request") {
		t.Errorf("notes = %v, want one naming --pull-request", e.Data.Notes)
	}
	if a.ExpiresAt == "" {
		t.Error("expires_at is empty, but a keyless upload is ephemeral")
	}
	// The claim token is the only way to keep it, so it must survive to the output.
	if !strings.HasPrefix(a.ClaimToken, "krowk_claim_") {
		t.Errorf("claim_token = %q", a.ClaimToken)
	}
	crumb, _ := findCrumb(e.Breadcrumbs, "krowk claim ")
	if crumb.Cmd != "krowk claim "+a.Slug+" "+a.ClaimToken {
		t.Errorf("claim breadcrumb = %q", crumb.Cmd)
	}
}

func TestClaimKeepsAnAnonymousUploadPastExpiry(t *testing.T) {
	h := newHarness(t, 0)

	// Upload with no key, then claim with one — the two halves of the flow the
	// registry offers a first-time user.
	delete(h.env, "KROWK_TOKEN")
	uploaded := only(t, h.ok("push", h.fixture))
	h.env["KROWK_TOKEN"] = "krowk_sk_test"

	claimed := only(t, h.ok("claim", uploaded.Slug, uploaded.ClaimToken))
	if claimed.Slug != uploaded.Slug {
		t.Errorf("claimed %q, want %q", claimed.Slug, uploaded.Slug)
	}
	if claimed.ExpiresAt != "" {
		t.Errorf("expires_at = %q, want empty once claimed", claimed.ExpiresAt)
	}
	// The link must not move: the whole point is that it has already been pasted.
	if claimed.URL != uploaded.URL {
		t.Errorf("claiming moved the link: %q → %q", uploaded.URL, claimed.URL)
	}

	// Claiming again with the same key is the same success, because an agent that
	// retries after a claim that worked should not see an error.
	if again := only(t, h.ok("claim", uploaded.Slug, uploaded.ClaimToken)); again.Slug != uploaded.Slug {
		t.Errorf("re-claiming gave %q", again.Slug)
	}

	// A token is good once, though: someone else holding it gets nothing, and
	// gets the same answer a wrong token gets.
	h.env["KROWK_TOKEN"] = "krowk_sk_someone_else"
	if got := h.fails("claim", uploaded.Slug, uploaded.ClaimToken)["error"]; got != "not_found" {
		t.Errorf("a spent token gave %v, want not_found", got)
	}
	if got := h.fails("claim", uploaded.Slug, "krowk_claim_wrong")["error"]; got != "not_found" {
		t.Errorf("a wrong token gave %v, want not_found", got)
	}
}

// An upload that started out anonymous is the case with no other way out: it
// could not name a run when it was created, and claiming it does not give it one.
// So claim carries --run, and does the two halves in the only order that works.
func TestClaimAttachesTheAdoptedUploadToARun(t *testing.T) {
	h := newHarness(t, 0)
	started := h.ok("runs", "start", "--session=sess_xyz")

	h.anonymous()
	uploaded := only(t, h.ok("push", h.fixture))
	h.env["KROWK_TOKEN"] = "krowk_sk_test"

	e := h.ok("claim", uploaded.Slug, uploaded.ClaimToken, "--run="+started.Data.Slug)
	claimed := only(t, e)
	if claimed.runSlug() != started.Data.Slug {
		t.Errorf("claimed artifact run = %q, want %q", claimed.runSlug(), started.Data.Slug)
	}
	// The run is what the flag was for, so the line an agent reads first says so.
	if !strings.Contains(e.Summary, "run "+started.Data.Slug) {
		t.Errorf("summary = %q, want the run named", e.Summary)
	}
	if claimed.ExpiresAt != "" {
		t.Errorf("expires_at = %q, want empty once claimed", claimed.ExpiresAt)
	}
	// The link is what has already been pasted, so neither half may move it.
	if claimed.URL != uploaded.URL {
		t.Errorf("claiming moved the link: %q → %q", uploaded.URL, claimed.URL)
	}
}

// A run the claim cannot reach fails after the token is spent. The artifact is
// kept either way, and saying so is the difference between a caller retrying the
// attach and a caller believing the upload is gone.
func TestAFailedAttachSaysTheClaimStillLanded(t *testing.T) {
	h := newHarness(t, 0)

	h.anonymous()
	uploaded := only(t, h.ok("push", h.fixture))
	h.env["KROWK_TOKEN"] = "krowk_sk_test"

	body := h.fails("claim", uploaded.Slug, uploaded.ClaimToken, "--run=run_nosuchrunatall00000")
	if body["error"] != "not_found" {
		t.Fatalf("error = %v, want not_found", body)
	}
	if body["claimed"] != uploaded.Slug {
		t.Errorf("claimed = %v, want %q", body["claimed"], uploaded.Slug)
	}
	fix, _ := body["fix"].(string)
	// The artifact slug is named, because the error body is all the caller sees of
	// it. The run that just 404'd is not quoted back as the one to retry with — the
	// same sentence says to check the slug, and it must not then hand it over.
	if !strings.Contains(fix, "uploads attach "+uploaded.Slug) {
		t.Errorf("fix = %q, want the attach to retry", fix)
	}
	if strings.Contains(fix, "--run run_nosuchrunatall00000") {
		t.Errorf("fix = %q, want the failed run left as a placeholder", fix)
	}

	// And the claim really did land: the artifact is in the workspace, unexpiring.
	if shown := only(t, h.ok("uploads", "show", uploaded.Slug)); shown.ExpiresAt != "" {
		t.Errorf("expires_at = %q, want the claim to have stuck", shown.ExpiresAt)
	}
}

// The same attach on its own, for an upload the key already owns.
func TestUploadsAttachPutsAnUploadUnderARun(t *testing.T) {
	h := newHarness(t, 0)
	started := h.ok("runs", "start")
	pushed := only(t, h.ok("push", h.fixture, "--run="+started.Data.Slug))

	other := h.ok("runs", "start")
	attached := only(t, h.ok("uploads", "attach", pushed.Slug, "--run="+other.Data.Slug))
	if attached.runSlug() != other.Data.Slug {
		t.Errorf("attach = %q, want %q", attached.runSlug(), other.Data.Slug)
	}
	// Idempotent, because agents retry.
	if again := only(t, h.ok("uploads", "attach", pushed.Slug, "--run="+other.Data.Slug)); again.runSlug() != other.Data.Slug {
		t.Errorf("second attach = %q", again.runSlug())
	}
}

func TestUploadsAttachNeedsBothHalves(t *testing.T) {
	h := newHarness(t, 0)
	pushed := only(t, h.ok("push", h.fixture))

	if got := h.fails("uploads", "attach")["error"]; got != "no_artifact" {
		t.Errorf("no artifact gave %v, want no_artifact", got)
	}
	body := h.fails("uploads", "attach", pushed.Slug)
	if body["error"] != "no_run" {
		t.Errorf("no --run gave %v, want no_run", body["error"])
	}
	if fix, _ := body["fix"].(string); !strings.Contains(fix, "--run") {
		t.Errorf("fix = %q, want it to name the flag", fix)
	}
}

// Attaching needs a key the ordinary way, so a keyless one is a plain 401 — not
// the run_needs_key a keyless push naming a run gets, which the registry raises
// only where a request may legitimately arrive without a key. Pinned because the
// difference is what a client branches on, and a friendlier stand-in here would
// hide it until production.
func TestUploadsAttachNeedsAKey(t *testing.T) {
	h := newHarness(t, 0)
	started := h.ok("runs", "start")
	pushed := only(t, h.ok("push", h.fixture))

	h.anonymous()
	body := h.fails("uploads", "attach", pushed.Slug, "--run="+started.Data.Slug)
	if body["error"] != "unauthorized" {
		t.Fatalf("error = %v, want unauthorized", body)
	}
	// With no key sent at all, "check your token" is no advice; naming the login is.
	if fix, _ := body["fix"].(string); !strings.Contains(fix, "auth login") {
		t.Errorf("fix = %q, want it to name the login", fix)
	}
}

func TestRunsStartGroupsLaterUploads(t *testing.T) {
	h := newHarness(t, 0)

	started := h.ok("runs", "start", "--session=sess_xyz")
	if started.Data.Slug == "" || started.Data.Status != "open" {
		t.Fatalf("runs start = %+v", started.Data)
	}
	if got := started.Data.Metadata["session"]; got != "sess_xyz" {
		t.Errorf("run metadata session = %v", got)
	}

	e := h.ok("push", h.fixture, "--run="+started.Data.Slug)
	if got := only(t, e).runSlug(); got != started.Data.Slug {
		t.Errorf("artifact run = %q, want %q", got, started.Data.Slug)
	}
	// A run the caller opened is a run the caller closes, so pushing to it must
	// leave it open.
	if e.Data.Run != nil {
		t.Errorf("push reported run %+v, but it did not open one", e.Data.Run)
	}

	finished := h.ok("runs", "finish", started.Data.Slug)
	if finished.Data.Status != "finished" {
		t.Errorf("runs finish = %+v", finished.Data)
	}
	// Idempotent, because a CI cleanup step runs twice.
	if again := h.ok("runs", "finish", started.Data.Slug); again.Data.Status != "finished" {
		t.Errorf("second finish = %+v", again.Data)
	}
}

func TestAttachingToARunNeedsAKey(t *testing.T) {
	h := newHarness(t, 0)
	started := h.ok("runs", "start")

	h.anonymous()
	body := h.fails("push", h.fixture, "--run="+started.Data.Slug)
	if body["error"] != "run_needs_key" {
		t.Fatalf("error = %v, want run_needs_key", body)
	}
	if fix, _ := body["fix"].(string); !strings.Contains(fix, "API key") {
		t.Errorf("fix = %q, want it to name the key", fix)
	}
}

func TestUploadsShowReadsAnArtifactBack(t *testing.T) {
	h := newHarness(t, 0)
	pushed := only(t, h.ok("push", h.fixture))

	shown := only(t, h.ok("uploads", "show", pushed.Slug))
	if shown.Slug != pushed.Slug || shown.URL != pushed.URL || shown.State != "ready" {
		t.Errorf("show = %+v, want the pushed artifact", shown)
	}

	if got := h.fails("uploads", "show", "art_nope")["error"]; got != "not_found" {
		t.Errorf("unknown slug gave %v, want not_found", got)
	}
}

// A slug is not a capability across workspaces: another key must not be able to
// read an artifact by guessing at it.
func TestUploadsShowIsScopedToTheKey(t *testing.T) {
	h := newHarness(t, 0)
	pushed := only(t, h.ok("push", h.fixture))

	h.env["KROWK_TOKEN"] = "krowk_sk_someone_else"
	if got := h.fails("uploads", "show", pushed.Slug)["error"]; got != "not_found" {
		t.Errorf("another key read it: %v", got)
	}
}

func TestUploadsListPagesNewestFirst(t *testing.T) {
	h := newHarness(t, 0)
	for _, name := range []string{"one.txt", "two.txt", "three.txt"} {
		h.ok("push", h.write(name, "contents of "+name))
	}

	all := h.ok("uploads", "list")
	if len(all.Data.Artifacts) != 3 {
		t.Fatalf("want 3 artifacts, got %d", len(all.Data.Artifacts))
	}
	if got := all.Data.Artifacts[0].Filename; got != "three.txt" {
		t.Errorf("first = %q, want three.txt — newest first", got)
	}

	// A full page carries the cursor for the next one.
	first := h.ok("uploads", "list", "--limit=2")
	if len(first.Data.Artifacts) != 2 {
		t.Fatalf("--limit=2 returned %d", len(first.Data.Artifacts))
	}
	cursor := first.Data.Next
	if cursor == "" {
		t.Fatal("a full page should carry a next cursor")
	}

	second := h.ok("uploads", "list", "--limit=2", "--before="+cursor)
	if len(second.Data.Artifacts) != 1 {
		t.Fatalf("second page returned %d, want the remaining 1", len(second.Data.Artifacts))
	}
	if second.Data.Artifacts[0].Filename != "one.txt" {
		t.Errorf("second page = %q, want one.txt", second.Data.Artifacts[0].Filename)
	}
	// Not a full page, so there is nothing behind it.
	if second.Data.Next != "" {
		t.Errorf("last page carries next = %q", second.Data.Next)
	}
}

// Listing is the one artifact endpoint that refuses a keyless request: every
// keyless upload shares one workspace, so listing it would show everyone's.
func TestUploadsListNeedsAKey(t *testing.T) {
	h := newHarness(t, 0).anonymous()

	body := h.fails("uploads", "list")
	if body["error"] != "unauthorized" {
		t.Errorf("error = %v, want unauthorized", body)
	}
	if fix, _ := body["fix"].(string); !strings.Contains(fix, "KROWK_TOKEN") {
		t.Errorf("fix = %q", fix)
	}
}

func TestFlagsMayFollowFilenames(t *testing.T) {
	h := newHarness(t, 0)

	e := h.ok("uploads", "create", h.fixture, "--session=after-the-file")
	if e.Data.Run == nil || e.Data.Run.Metadata["session"] != "after-the-file" {
		t.Errorf("session = %v", e.Data.Run)
	}
}

func TestMarkdownFormatIsPasteReady(t *testing.T) {
	h := newHarness(t, 0)

	_, stdout, _ := h.run("push", h.fixture, "--title=Checkout", "--format=markdown")

	// An image embeds and the title labels it — and the embed names the bytes
	// while the link around it names the card page, which is the only pair of
	// URLs that both renders in a pull request and clicks through to the run.
	want := regexp.MustCompile(`^\[!\[Checkout]\(http.+checkout-after\.png\)]\(http.+/a/art_\w+\)$`)
	if !want.MatchString(strings.TrimSpace(stdout)) {
		t.Errorf("markdown = %q", stdout)
	}
}

func TestMarkdownGivesOneLinePerFile(t *testing.T) {
	h := newHarness(t, 0)
	second := h.write("log.txt", "the build log")

	_, stdout, _ := h.run("push", h.fixture, second, "--format=markdown")

	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 {
		t.Fatalf("want a line per file, got %q", stdout)
	}
	// The image embeds; the log is a plain link. Both are wrapped in, or are,
	// a link to the card page.
	if !strings.HasPrefix(lines[0], "[![") || !strings.HasPrefix(lines[1], "[log.txt](") {
		t.Errorf("markdown = %q", stdout)
	}
	for i, line := range lines {
		if !strings.HasSuffix(line, ")") || !strings.Contains(line, "/a/art_") {
			t.Errorf("line %d = %q, want it to link to a card page", i, line)
		}
	}
}

func TestMissingFileFailsBeforeUploading(t *testing.T) {
	h := newHarness(t, 0)
	h.env["KROWK_API_URL"] = "http://127.0.0.1:1/v1" // any call would fail loudly

	body := h.fails("push", "nope.png")
	if body["error"] != "file_unreadable" || body["fix"] == nil {
		t.Errorf("error = %v, want file_unreadable with a fix", body)
	}
}

// Nothing is uploaded until every path has been read, so one bad path in a batch
// cannot leave half the files up.
func TestOneUnreadablePathStopsTheWholeBatch(t *testing.T) {
	h := newHarness(t, 0)

	if body := h.fails("push", h.fixture, "nope.png"); body["error"] != "file_unreadable" {
		t.Fatalf("error = %v", body)
	}
	if e := h.ok("runs", "start"); e.Data.Slug == "" {
		t.Fatal("registry unusable after the failure")
	}
}

func TestEmptyFileIsRefusedLocally(t *testing.T) {
	h := newHarness(t, 0)
	empty := h.write("empty.png", "")

	body := h.fails("push", empty)
	if body["error"] != "empty_file" {
		t.Errorf("error = %v, want empty_file", body)
	}
}

func TestOversizedUploadSurfacesTheLimit(t *testing.T) {
	h := newHarness(t, 10) // 10-byte cap, so the fixture is too large

	body := h.fails("push", h.fixture)
	if body["error"] != "invalid" || body["status"] != float64(422) {
		t.Fatalf("error = %v, want invalid with a 422", body)
	}
	if msg, _ := body["message"].(string); !strings.Contains(msg, "10 bytes") {
		t.Errorf("message = %q, want the limit in it", msg)
	}
	// The agent has to stop rather than retry: the file will not get smaller.
	if body["retryable"] != false {
		t.Errorf("retryable = %v, want false", body["retryable"])
	}
}

func TestUnreachableRegistryIsAnActionableError(t *testing.T) {
	h := newHarness(t, 0)
	h.env["KROWK_API_URL"] = "http://127.0.0.1:1/v1"

	body := h.fails("push", h.fixture)
	fix, _ := body["fix"].(string)
	if body["error"] != "network_unreachable" || !strings.Contains(fix, "KROWK_API_URL") {
		t.Errorf("error = %v", body)
	}
}

func TestDoctorNamesTheServiceItReached(t *testing.T) {
	h := newHarness(t, 0)

	_, stdout, _ := h.run("doctor")
	var report map[string]any
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("doctor is not JSON: %v\n%s", err, stdout)
	}
	if status, _ := report["api_status"].(string); !strings.Contains(status, "krowk-registry") {
		t.Errorf("api_status = %q, want the service named", status)
	}
	if report["authenticated"] != true || report["runs_available"] != true {
		t.Errorf("doctor = %v", report)
	}
}

func TestDoctorReportsAWrongURL(t *testing.T) {
	h := newHarness(t, 0)
	h.env["KROWK_API_URL"] = "http://127.0.0.1:1/v1"

	_, stdout, _ := h.run("doctor")
	if !strings.Contains(stdout, "network_unreachable") {
		t.Errorf("doctor = %s", stdout)
	}
}

func TestUnknownCommandIsReadable(t *testing.T) {
	h := newHarness(t, 0)

	if got := h.fails("uploads", "yeet")["error"]; got != "unknown_command" {
		t.Errorf("error = %v", got)
	}
}

// The registry's API is resourceful, and the verb carries meaning: finalizing and
// completing are idempotent so they are PUTs, claiming spends a one-shot token so
// it is a POST. Getting either wrong is a 404 against the real registry and a
// green suite against a stand-in that drifted with the client — so the calls are
// pinned here, against `bin/rails routes`, rather than only exercised.
func TestWireShapeMatchesTheRegistrysRoutes(t *testing.T) {
	var mu sync.Mutex
	var calls []string

	inner := registry.Handler(0, "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if call := wireCall(r); call != "" {
			mu.Lock()
			calls = append(calls, call)
			mu.Unlock()
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	isolateConfig(t)

	h := &harness{t: t, server: server, env: map[string]string{
		"KROWK_API_URL": server.URL + "/v1",
		"KROWK_TOKEN":   "krowk_sk_test",
	}}
	h.fixture = h.write("shot.png", "some bytes")

	pushed := only(t, h.ok("push", h.fixture))
	h.ok("uploads", "list")
	h.ok("uploads", "show", pushed.Slug)
	h.ok("runs", "list")
	h.ok("runs", "show", pushed.runSlug())
	// A run's artifacts are a collection of the run, not a filter on the listing
	// above — so --run is a different endpoint rather than a query parameter.
	h.ok("uploads", "list", "--run="+pushed.runSlug())
	h.run("doctor")

	// Claiming needs an anonymous artifact to claim.
	delete(h.env, "KROWK_TOKEN")
	anonymous := only(t, h.ok("push", h.fixture))
	h.env["KROWK_TOKEN"] = "krowk_sk_test"
	h.ok("claim", anonymous.Slug, anonymous.ClaimToken)
	h.ok("uploads", "attach", anonymous.Slug, "--run="+pushed.runSlug())
	// Taking it down again: a claimed artifact answers to the key that holds it.
	h.ok("uploads", "delete", anonymous.Slug)

	want := []string{
		// push, with a key: open a run, declare, finalize, close the run. The two
		// creates name their attempt so a retry of either costs nothing; +key marks
		// the ones that carry an Idempotency-Key, and only those two do.
		"POST /v1/runs +key",
		"POST /v1/artifacts +key",
		"PUT /v1/artifacts/{slug}/finalization",
		"PUT /v1/runs/{slug}/completion",
		"GET /v1/artifacts",
		"GET /v1/artifacts/{slug}",
		"GET /v1/runs",
		"GET /v1/runs/{slug}",
		"GET /v1/runs/{slug}/artifacts",
		// doctor: the reachability probe, then the key check.
		"GET /",
		"GET /v1/key",
		// push, keyless: no run to open or close.
		"POST /v1/artifacts +key",
		"PUT /v1/artifacts/{slug}/finalization",
		"POST /v1/artifacts/{slug}/claim",
		// and the run it never had, set afterwards.
		"PUT /v1/artifacts/{slug}/run",
		// Takedown is the REST delete of the artifact, not a nested resource:
		// destroying it is what the verb already means.
		"DELETE /v1/artifacts/{slug}",
	}

	mu.Lock()
	got := calls
	mu.Unlock()

	if len(got) != len(want) {
		t.Fatalf("calls:\n  %s\nwant:\n  %s",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, got[i], want[i])
		}
	}
}

var slugPattern = regexp.MustCompile(`(art|run|ws)_[0-9A-Za-z]+`)

func normalizeSlugs(path string) string {
	return slugPattern.ReplaceAllString(path, "{slug}")
}

// wireCall names one request the way the pins read it, and answers empty for a
// request that is not part of the registry's API surface — object storage is not.
//
// The Idempotency-Key is part of the shared contract, so it is pinned with the
// method and the path rather than tested somewhere off to the side: a create that
// stops naming its attempt goes back to charging for every retry, and nothing else
// here would notice.
func wireCall(r *http.Request) string {
	if strings.HasPrefix(r.URL.Path, "/_storage") {
		return ""
	}
	call := r.Method + " " + normalizeSlugs(r.URL.Path)
	if r.Header.Get("Idempotency-Key") != "" {
		call += " +key"
	}
	return call
}

// clock is a movable now, which is the only way a test reaches an expiry: a
// 15-minute signature window is not going to elapse inside one. Guarded, because
// the registry reads it from the server's goroutine while the CLI runs in this
// one.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// lapsingHarness runs the CLI against a registry whose presigned URL is dead by
// the time the bytes reach it: the clock jumps past the signature's window as the
// declare is answered. That is the shape of an agent that digested a large file,
// met a slow network, or came back to a push after a crash — the artifact is still
// waiting, and only the signature over it has gone stale.
//
// calls answers the API surface the push touched, as the wire-shape pin reads it.
func lapsingHarness(t *testing.T) (h *harness, calls func() []string) {
	t.Helper()

	var mu sync.Mutex
	var recorded []string
	clk := &clock{at: time.Now()}
	inner := registry.HandlerWithClock(0, "", clk.now)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if call := wireCall(r); call != "" {
			mu.Lock()
			recorded = append(recorded, call)
			mu.Unlock()
		}
		declaring := r.Method == http.MethodPost && r.URL.Path == "/v1/artifacts"
		inner.ServeHTTP(w, r)
		// After the answer, so the URL the client is handed advertises the window
		// it was minted with — and is past it by the time the PUT arrives.
		if declaring {
			clk.advance(registry.UploadURLLifetime + time.Minute)
		}
	}))
	t.Cleanup(server.Close)
	isolateConfig(t)

	h = &harness{t: t, server: server, env: map[string]string{
		"KROWK_API_URL": server.URL + "/v1",
		"KROWK_TOKEN":   "krowk_sk_test",
	}}
	h.fixture = h.write("checkout-after.png", "fake png bytes for the test")

	return h, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), recorded...)
	}
}

// A push whose bytes meet a lapsed signature recovers by minting a fresh one over
// the same artifact. It matters that it is the same one: the alternative is
// declaring the file again, and that is a second slug — so the link the first push
// printed, and whatever it was already pasted into, would resolve to nothing
// forever.
//
// This also pins the wire shape of the recovery call, which the happy-path pin
// never sees: a represign fires on a failed upload only.
func TestPushRecoversFromALapsedSignatureUnderOneSlug(t *testing.T) {
	h, calls := lapsingHarness(t)

	pushed := only(t, h.ok("push", h.fixture))
	got := calls()

	if pushed.State != "ready" {
		t.Fatalf("state = %q, want the upload to have completed", pushed.State)
	}
	// The link is the product: it has to serve the bytes that were pushed.
	if status, body := h.get(pushed.FileURL); status != 200 || body != "fake png bytes for the test" {
		t.Errorf("GET %s = %d %q", pushed.FileURL, status, body)
	}

	want := []string{
		"POST /v1/runs +key",
		"POST /v1/artifacts +key",
		// The recovery. No Idempotency-Key: it creates nothing, it re-mints a
		// capability over a record that already exists.
		"POST /v1/artifacts/{slug}/upload",
		"PUT /v1/artifacts/{slug}/finalization",
		"PUT /v1/runs/{slug}/completion",
	}
	if len(got) != len(want) {
		t.Fatalf("calls:\n  %s\nwant:\n  %s",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// The keyless half of the same recovery, and the one that needs the claim token:
// there is no key to fall back on, and the registry will not re-presign on the
// slug alone. The client is holding the token — it came back with the slug — so
// the recovery is available to exactly the caller the one-shot flow is built for.
func TestKeylessPushRecoversFromALapsedSignature(t *testing.T) {
	h, calls := lapsingHarness(t)
	h.anonymous()

	pushed := only(t, h.ok("push", h.fixture))
	got := calls()

	if pushed.State != "ready" {
		t.Fatalf("state = %q, want the upload to have completed", pushed.State)
	}
	// Still the only way to keep it, and the recovery must not have cost it.
	if !strings.HasPrefix(pushed.ClaimToken, "krowk_claim_") {
		t.Errorf("claim_token = %q, want the one the declare handed over", pushed.ClaimToken)
	}
	if status, body := h.get(pushed.FileURL); status != 200 || body != "fake png bytes for the test" {
		t.Errorf("GET %s = %d %q", pushed.FileURL, status, body)
	}
	// One declare, so one slug: the recovery went through the artifact that was
	// already declared rather than round it.
	want := []string{
		"POST /v1/artifacts +key",
		"POST /v1/artifacts/{slug}/upload",
		"PUT /v1/artifacts/{slug}/finalization",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls:\n  %s\nwant:\n  %s",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestHelpAndVersion(t *testing.T) {
	h := newHarness(t, 0)

	if code, stdout, _ := h.run("--version"); code != 0 || strings.TrimSpace(stdout) != Version {
		t.Errorf("--version = %q (exit %d)", stdout, code)
	}
	if code, stdout, _ := h.run(); code != 0 || !strings.Contains(stdout, "krowk push") {
		t.Errorf("bare invocation should print help, got %q (exit %d)", stdout, code)
	}
}

// faultyHarness is a harness whose registry drops one request, chosen by the
// caller — the way tests reproduce a mid-batch failure without a mid-batch bug.
func faultyHarness(t *testing.T, breaks func(r *http.Request, n int) bool) *harness {
	t.Helper()

	var mu sync.Mutex
	var n int
	inner := registry.Handler(0, "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		broken := breaks(r, n)
		mu.Unlock()
		if broken {
			// 400 rather than 500, so the client does not sit out retry backoffs —
			// and rather than 403, which on the storage leg is what a lapsed
			// signature looks like: the client answers that by minting a fresh URL
			// and sending the bytes again, so a test that wants an upload to stay
			// dead has to break it in a way that is not that.
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"code":"injected_fault","message":"injected fault"}}`)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)

	h := &harness{t: t, server: server, env: map[string]string{
		"KROWK_API_URL": server.URL + "/v1",
		"KROWK_TOKEN":   "krowk_sk_test",
	}}
	h.fixture = h.write("checkout-after.png", "fake png bytes for the test")
	return h
}

// A keyed push that dies mid-batch opened a run, and the error body is the only
// place its slug can survive — without it the run can never be finished.
func TestFailedKeyedPushNamesItsRunAndKeepsProgress(t *testing.T) {
	var puts int
	h := faultyHarness(t, func(r *http.Request, n int) bool {
		if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/_storage") {
			puts++
			return puts == 2
		}
		return false
	})
	second := h.write("log.txt", "the build log")
	third := h.write("trace.txt", "the trace")

	body := h.fails("push", h.fixture, second, third)

	slug, _ := body["run"].(string)
	if !strings.HasPrefix(slug, "run_") {
		t.Fatalf("run = %v, want the slug of the run the push opened", body["run"])
	}
	if fix, _ := body["fix"].(string); !strings.Contains(fix, "krowk runs finish "+slug) {
		t.Errorf("fix = %q, want it to name `krowk runs finish %s`", fix, slug)
	}
	// The first file made it up before the second died, and its link still
	// works. The link is the card page — that is what an agent would hand on —
	// so what is checked is that the page resolves and names the file, not that
	// the URL serves bytes.
	urls, _ := body["uploaded_before_failure"].([]any)
	if len(urls) != 1 {
		t.Fatalf("uploaded_before_failure = %v, want the first file's URL", body["uploaded_before_failure"])
	}
	status, page := h.get(urls[0].(string))
	if status != 200 || !strings.Contains(page, "checkout-after.png") {
		t.Errorf("GET %v = %d %q", urls[0], status, page)
	}
	// The slug in the error is enough to actually close the run.
	if e := h.ok("runs", "finish", slug); e.Data.Status != "finished" {
		t.Errorf("runs finish = %+v", e.Data)
	}
}

// Failing to close the run must not fail the upload — but it must not be
// silent either: the caller is told the run is open and how to finish it.
func TestUnfinishedRunIsReportedNotSwallowed(t *testing.T) {
	h := faultyHarness(t, func(r *http.Request, n int) bool {
		return strings.HasSuffix(r.URL.Path, "/completion")
	})

	e := h.ok("push", h.fixture)
	if e.Data.Run == nil || e.Data.Run.Status != "open" {
		t.Fatalf("run = %+v, want it reported as it was last known: open", e.Data.Run)
	}
	note := strings.Join(e.Data.Notes, "\n")
	if !strings.Contains(note, "krowk runs finish "+e.Data.Run.Slug) {
		t.Errorf("notes = %q, want the retry command with the run's slug", note)
	}
}

// Opening a run used to get exactly one attempt: it is a POST, and a run
// committed under a lost response would be duplicated by the retry, orphaning the
// first slug. An Idempotency-Key removes that trade — the retry is answered with
// the run the first attempt opened — so this is retried like anything else, and a
// transient failure on the way to opening a run no longer costs the whole upload.
//
// Every attempt has to carry the same key. A retry under a fresh one is a second
// create by another name, and the duplicate this used to avoid is back.
func TestCreateRunIsRetriedUnderOneKey(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/runs" {
			mu.Lock()
			keys = append(keys, r.Header.Get("Idempotency-Key"))
			mu.Unlock()
		}
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"code":"boom","message":"transient"}}`)
	}))
	t.Cleanup(server.Close)
	isolateConfig(t)

	h := &harness{t: t, server: server, env: map[string]string{
		"KROWK_API_URL": server.URL + "/v1",
		"KROWK_TOKEN":   "krowk_sk_test",
	}}
	h.fixture = h.write("shot.png", "some bytes")

	h.fails("push", h.fixture)
	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 3 {
		t.Fatalf("POST /v1/runs was sent %d times, want 3 attempts", len(keys))
	}
	for i, key := range keys {
		if key == "" {
			t.Fatalf("attempt %d carried no Idempotency-Key", i+1)
		}
		if key != keys[0] {
			t.Errorf("attempt %d used key %q, want the first attempt's %q", i+1, key, keys[0])
		}
	}
}

// next_step is an instruction the registry hands to agents; if it names a route
// that does not exist, doing literally what it says answers 404. The wire-shape
// test pins the calls the client makes — this pins the instruction itself.
func TestNextStepNamesTheRealFinalizationRoute(t *testing.T) {
	h := newHarness(t, 0)

	res, err := http.Post(h.server.URL+"/v1/artifacts", "application/json",
		strings.NewReader(`{"artifact":{"filename":"a.png","content_type":"image/png","byte_size":4}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var payload struct {
		Slug     string `json:"slug"`
		NextStep string `json:"next_step"`
	}
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if want := "PUT /v1/artifacts/" + payload.Slug + "/finalization"; !strings.Contains(payload.NextStep, want) {
		t.Errorf("next_step = %q, want it to name %q", payload.NextStep, want)
	}
}

// A ready artifact is a permalink; its presigned URL must not keep working
// after finalization, or a re-PUT could silently swap the stored bytes.
func TestReadyArtifactRejectsAnotherUpload(t *testing.T) {
	h := newHarness(t, 0)

	a := only(t, h.ok("push", h.fixture))

	// The upload URL is not surfaced by the CLI after a push, so reconstruct the
	// PUT the way an attacker holding a leaked token would: any token is as good
	// as none once finalization has spent it.
	req, err := http.NewRequest(http.MethodPut, a.FileURL+"?upload_token=anything", strings.NewReader("swapped"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", a.ContentType)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("re-PUT after finalize = %d, want 403", res.StatusCode)
	}
	if _, body := h.get(a.FileURL); body != "fake png bytes for the test" {
		t.Errorf("stored bytes changed to %q", body)
	}
}

// A takedown is what someone reaches for when a secret was published by
// accident, so the whole of it has to hold: the bytes stop serving, and the link
// reports that it was taken down rather than that it never existed.
func TestUploadsDeleteTakesTheBytesDownAndLeavesATombstone(t *testing.T) {
	h := newHarness(t, 0)

	pushed := only(t, h.ok("push", h.fixture))
	if status, _ := h.get(pushed.FileURL); status != http.StatusOK {
		t.Fatalf("the upload never served: %d", status)
	}

	e := h.ok("uploads", "delete", pushed.Slug)
	if !e.OK || e.Data.Slug != pushed.Slug || !e.Data.TakenDown {
		t.Errorf("delete = %+v", e)
	}

	if status, _ := h.get(pushed.FileURL); status != http.StatusNotFound {
		t.Errorf("GET %s after takedown = %d, want 404", pushed.FileURL, status)
	}
	if got := h.fails("uploads", "show", pushed.Slug)["error"]; got != "taken_down" {
		t.Errorf("show after takedown = %v, want taken_down", got)
	}
	// A tombstone is not something the workspace still holds.
	if artifacts := h.ok("uploads", "list").Data.Artifacts; len(artifacts) != 0 {
		t.Errorf("listing still holds the tombstone: %+v", artifacts)
	}
}

// 410 is not 404 on purpose, so the client has to have something to say about
// it: the artifact existed and is gone, rather than a slug worth re-checking.
func TestTakenDownReadsAsGoneForGoodRatherThanAsATypo(t *testing.T) {
	h := newHarness(t, 0)

	pushed := only(t, h.ok("push", h.fixture))
	h.ok("uploads", "delete", pushed.Slug)

	body := h.fails("uploads", "show", pushed.Slug)
	fix, _ := body["fix"].(string)
	if !strings.Contains(fix, "taken down") || !strings.Contains(fix, "upload it again") {
		t.Errorf("fix = %q, want it to say the artifact is gone and uploading again is the way back", fix)
	}
	// Nothing about retrying changes the answer, and an agent that keeps trying
	// is worse than one that stops. The verdict has to be stated, not merely
	// absent: a missing key reads as false to anything asserting on it, so
	// "not retryable" and "nothing said" would look identical here.
	retryable, stated := body["retryable"].(bool)
	if !stated || retryable {
		t.Errorf("retryable = %v (stated %v), want an explicit false", retryable, stated)
	}
}

// The case that decides how the request is made. An agent pushes anonymously
// from CI; the person cleaning up is logged in. Offered alongside a key the
// registry reads the key and nothing else, and looks in that key's workspace —
// where an artifact still sitting in the anonymous one is simply not found. So a
// claim token has to be sent as the authority it is, with the key withheld.
func TestClaimTokenTakesDownAnAnonymousUploadFromALoggedInMachine(t *testing.T) {
	h := newHarness(t, 0)

	anonymous := only(t, h.anonymous().ok("push", h.fixture))
	if anonymous.ClaimToken == "" {
		t.Fatal("an anonymous push should come back with a claim token")
	}
	h.env["KROWK_TOKEN"] = "krowk_sk_test"

	e := h.ok("uploads", "delete", anonymous.Slug, anonymous.ClaimToken)
	if !e.Data.TakenDown {
		t.Errorf("delete = %+v", e)
	}
	if status, _ := h.get(anonymous.FileURL); status != http.StatusNotFound {
		t.Errorf("GET %s after takedown = %d, want 404", anonymous.FileURL, status)
	}
}

// Without a key and without a token there is no authority at all, and saying so
// beats a 400 from the registry naming a parameter the caller never saw.
func TestDeletingAnonymouslyWithoutATokenNamesWhatIsMissing(t *testing.T) {
	h := newHarness(t, 0).anonymous()

	pushed := only(t, h.ok("push", h.fixture))
	body := h.fails("uploads", "delete", pushed.Slug)
	if body["error"] != "missing_claim" {
		t.Errorf("error = %v, want missing_claim", body["error"])
	}
	// The upload is still there: refusing locally must not half-do the takedown.
	if status, _ := h.get(pushed.FileURL); status != http.StatusOK {
		t.Errorf("GET %s = %d, want the upload untouched", pushed.FileURL, status)
	}
}

// A 404 from a takedown covers three different mistakes, and which of them are
// possible depends on the authority the caller sent — so the advice has to name
// the one it actually used rather than the standing "check your key".
func TestTakedownNotFoundNamesTheAuthorityThatWasUsed(t *testing.T) {
	h := newHarness(t, 0)

	// Asserted on wording only this command produces. The registry's standing
	// not_found advice already mentions the key, so looking for "key" would pass
	// whether or not the keyed branch ran at all.
	byKey, _ := h.fails("uploads", "delete", "art_nosuchartifact00001")["fix"].(string)
	if !strings.Contains(byKey, "still anonymous") {
		t.Errorf("keyed fix = %q, want it to point at the claim token as the other way in", byKey)
	}
	byToken, _ := h.fails("uploads", "delete", "art_nosuchartifact00001", "krowk_claim_nope")["fix"].(string)
	if strings.Contains(byToken, "the key matches") {
		t.Errorf("token fix = %q, want it to talk about the token rather than the key", byToken)
	}
	if !strings.Contains(byToken, "token") {
		t.Errorf("token fix = %q, want it to name the token", byToken)
	}
}

// A second positional that is not a token is not a wrong token. Taking it as one
// would withhold the key, so `uploads delete art_a art_b` — meaning two
// artifacts — would quietly become an unauthorised takedown reported as a 404.
func TestASecondWordThatIsNotAClaimTokenIsRefused(t *testing.T) {
	h := newHarness(t, 0)

	first := only(t, h.ok("push", h.fixture))
	second := only(t, h.ok("push", h.write("second.png", "more bytes")))

	body := h.fails("uploads", "delete", first.Slug, second.Slug)
	if body["error"] != "bad_claim_token" {
		t.Errorf("error = %v, want bad_claim_token", body["error"])
	}
	// Neither is touched: a refusal here must not half-do the takedown.
	for _, a := range []artifact{first, second} {
		if status, _ := h.get(a.FileURL); status != http.StatusOK {
			t.Errorf("GET %s = %d, want the upload untouched", a.FileURL, status)
		}
	}
}

// A run listing of bare slugs is unreadable, and the metadata is where a run's
// identity actually lives — the registry keeps none on the artifact.
func TestRunsListNamesEachRunByWhatItWasFor(t *testing.T) {
	h := newHarness(t, 0)

	h.ok("push", h.fixture, "--title=Checkout — mobile")
	h.ok("push", h.fixture, "--repo=acme/storefront", "--commit=a1b2c3d")

	e := h.ok("runs", "list")
	if len(e.Data.Runs) != 2 {
		t.Fatalf("want two runs, got %d", len(e.Data.Runs))
	}
	// Newest first, so the second push leads.
	if e.Data.Runs[0].Metadata["repo"] != "acme/storefront" {
		t.Errorf("first row = %v, want the newest run", e.Data.Runs[0].Metadata)
	}

	_, human, _ := h.run("runs", "list", "--format=human")
	if !strings.Contains(human, "Checkout — mobile") || !strings.Contains(human, "acme/storefront") {
		t.Errorf("human listing = %q, want each run labelled by its metadata", human)
	}
}

// A run is the only place an upload's origin is recorded, so reading one back
// has to carry the whole of it.
func TestRunsShowCarriesTheMetadataAnUploadDoesNot(t *testing.T) {
	h := newHarness(t, 0)

	pushed := h.ok("push", h.fixture,
		"--pull-request=https://github.com/acme/storefront/pull/412",
		"--session=sess_abc123")
	runSlug := pushed.Data.Run.Slug

	e := h.ok("runs", "show", runSlug)
	if e.Data.Slug != runSlug || e.Data.Status != "finished" {
		t.Errorf("show = %+v", e.Data)
	}
	if e.Data.Metadata["pull_request"] != "https://github.com/acme/storefront/pull/412" {
		t.Errorf("metadata = %v", e.Data.Metadata)
	}

	_, human, _ := h.run("runs", "show", runSlug, "--format=human")
	for _, want := range []string{runSlug, "finished", "pull_request", "sess_abc123"} {
		if !strings.Contains(human, want) {
			t.Errorf("human detail missing %q:\n%s", want, human)
		}
	}
	if got := h.fails("runs", "show", "run_nosuchrun0000000000")["error"]; got != "not_found" {
		t.Errorf("unknown run = %v, want not_found", got)
	}
}

// --run narrows the listing to what one run produced. The registry serves that
// as a collection of the run, so an unknown run is a 404 rather than an empty
// page — which a caller could not tell apart from a run that made nothing.
func TestUploadsListNarrowsToOneRun(t *testing.T) {
	h := newHarness(t, 0)

	mine := h.ok("push", h.fixture, "--title=in the run")
	runSlug := mine.Data.Run.Slug
	h.ok("push", h.write("elsewhere.png", "other bytes")) // its own run

	if got := h.ok("uploads", "list").Data.Artifacts; len(got) != 2 {
		t.Fatalf("workspace listing = %d artifacts, want both", len(got))
	}

	scoped := h.ok("uploads", "list", "--run="+runSlug).Data.Artifacts
	if len(scoped) != 1 || scoped[0].Slug != only(t, mine).Slug {
		t.Errorf("run listing = %+v, want only what that run made", scoped)
	}

	if got := h.fails("uploads", "list", "--run=run_nosuchrun0000000000")["error"]; got != "not_found" {
		t.Errorf("unknown run = %v, want not_found rather than an empty page", got)
	}
}

// The cursor has to carry whatever scoped the page. Paging a run's uploads on
// without --run would silently widen to the whole workspace, which reads as the
// run having produced far more than it did.
func TestPagingARunsUploadsStaysScopedToTheRun(t *testing.T) {
	h := newHarness(t, 0)

	started := h.ok("runs", "start")
	runSlug := started.Data.Slug
	for _, name := range []string{"one.png", "two.png"} {
		h.ok("push", h.write(name, "bytes for "+name), "--run="+runSlug)
	}
	h.ok("push", h.write("unrelated.png", "not in the run"))

	e := h.ok("uploads", "list", "--run="+runSlug, "--limit=1")
	if e.Data.Next == "" {
		t.Fatal("want a cursor: the page came back full")
	}
	// Both the scope and the stride: a crumb that dropped --limit would page on
	// in 50s having been asked for 1, and one that dropped --run would widen to
	// the whole workspace.
	want := "krowk uploads list --run " + runSlug + " --limit 1 --before " + e.Data.Next
	crumb, ok := findCrumb(e.Breadcrumbs, "uploads list")
	if !ok || crumb.Cmd != want {
		t.Errorf("next-page breadcrumb = %+v, want %q", e.Breadcrumbs, want)
	}

	_, human, _ := h.run("uploads", "list", "--run="+runSlug, "--limit=1", "--format=human")
	if !strings.Contains(human, want) {
		t.Errorf("human cursor line = %q, want %q", human, want)
	}

	// And the command it suggests really does stay inside the run.
	second := h.ok("uploads", "list", "--run="+runSlug, "--before="+e.Data.Next)
	if len(second.Data.Artifacts) != 1 {
		t.Errorf("second page = %d artifacts, want the run's other one alone", len(second.Data.Artifacts))
	}
}
