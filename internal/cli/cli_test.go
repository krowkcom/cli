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
func (h *harness) fails(args ...string) map[string]any {
	h.t.Helper()
	code, stdout, stderr := h.run(args...)
	if code != 1 {
		h.t.Fatalf("`krowk %s` exited %d, want 1, stdout:\n%s", strings.Join(args, " "), code, stdout)
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
	OK          bool           `json:"ok"`
	Data        data           `json:"data"`
	Summary     string         `json:"summary"`
	Breadcrumbs []breadcrumb   `json:"breadcrumbs"`
	Error       map[string]any `json:"error"`
}

// data covers both shapes a command returns: an upload result, and a bare run.
type data struct {
	Artifacts []artifact     `json:"artifacts"`
	Run       *run           `json:"run"`
	Notes     []string       `json:"notes"`
	Next      string         `json:"next"`
	Slug      string         `json:"slug"`
	Status    string         `json:"status"`
	Metadata  map[string]any `json:"metadata"`
}

type artifact struct {
	Slug        string `json:"slug"`
	State       string `json:"state"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	ByteSize    int64  `json:"byte_size"`
	Checksum    string `json:"checksum"`
	Run         string `json:"run"`
	URL         string `json:"url"`
	Markdown    string `json:"markdown"`
	ExpiresAt   string `json:"expires_at"`
	ClaimToken  string `json:"claim_token"`
}

type run struct {
	Slug     string         `json:"slug"`
	Status   string         `json:"status"`
	Metadata map[string]any `json:"metadata"`
}

type breadcrumb struct {
	Action string `json:"action"`
	Cmd    string `json:"cmd"`
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
	status, body := h.get(a.URL)
	if status != 200 || body != "fake png bytes for the test" {
		t.Errorf("GET %s = %d %q", a.URL, status, body)
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
	if got := only(t, e).Run; got != e.Data.Run.Slug {
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
	refs, _ := meta["reference"].([]any)
	if len(refs) != 2 || refs[1] != "https://sentry.io/issues/1" {
		t.Errorf("reference = %v, want both links in order", meta["reference"])
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
	if e.Data.Run == nil || first.Run != last.Run || first.Run != e.Data.Run.Slug {
		t.Errorf("artifacts are not grouped under one run: %q and %q", first.Run, last.Run)
	}
	if last.ContentType != "text/plain" {
		t.Errorf("log.txt content type = %q", last.ContentType)
	}
	if _, body := h.get(last.URL); body != "the build log" {
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
	var claimCmd string
	for _, c := range e.Breadcrumbs {
		if strings.HasPrefix(c.Cmd, "krowk claim ") {
			claimCmd = c.Cmd
		}
	}
	if claimCmd != "krowk claim "+a.Slug+" "+a.ClaimToken {
		t.Errorf("claim breadcrumb = %q", claimCmd)
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
	if claimed.Run != started.Data.Slug {
		t.Errorf("claimed artifact run = %q, want %q", claimed.Run, started.Data.Slug)
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
	if !strings.Contains(fix, "uploads attach "+uploaded.Slug) {
		t.Errorf("fix = %q, want the attach to retry", fix)
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
	if attached.Run != other.Data.Slug {
		t.Errorf("attach = %q, want %q", attached.Run, other.Data.Slug)
	}
	// Idempotent, because agents retry.
	if again := only(t, h.ok("uploads", "attach", pushed.Slug, "--run="+other.Data.Slug)); again.Run != other.Data.Slug {
		t.Errorf("second attach = %q", again.Run)
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
	if got := only(t, e).Run; got != started.Data.Slug {
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

	// An image embeds, and the title labels it.
	want := regexp.MustCompile(`^!\[Checkout]\(http.+checkout-after\.png\)$`)
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
	// The image embeds; the log is a plain link.
	if !strings.HasPrefix(lines[0], "![") || !strings.HasPrefix(lines[1], "[log.txt]") {
		t.Errorf("markdown = %q", stdout)
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
		// Object storage is not part of the registry's API surface.
		if !strings.HasPrefix(r.URL.Path, "/_storage") {
			mu.Lock()
			calls = append(calls, r.Method+" "+normalizeSlugs(r.URL.Path))
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
	h.run("doctor")

	// Claiming needs an anonymous artifact to claim.
	delete(h.env, "KROWK_TOKEN")
	anonymous := only(t, h.ok("push", h.fixture))
	h.env["KROWK_TOKEN"] = "krowk_sk_test"
	h.ok("claim", anonymous.Slug, anonymous.ClaimToken)
	h.ok("uploads", "attach", anonymous.Slug, "--run="+pushed.Run)

	want := []string{
		// push, with a key: open a run, declare, finalize, close the run.
		"POST /v1/runs",
		"POST /v1/artifacts",
		"PUT /v1/artifacts/{slug}/finalization",
		"PUT /v1/runs/{slug}/completion",
		"GET /v1/artifacts",
		"GET /v1/artifacts/{slug}",
		// doctor: the reachability probe, then the key check.
		"GET /",
		"GET /v1/key",
		// push, keyless: no run to open or close.
		"POST /v1/artifacts",
		"PUT /v1/artifacts/{slug}/finalization",
		"POST /v1/artifacts/{slug}/claim",
		// and the run it never had, set afterwards.
		"PUT /v1/artifacts/{slug}/run",
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
			// 403 rather than 500, so the client does not sit out retry backoffs.
			w.WriteHeader(http.StatusForbidden)
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
	// The first file made it up before the second died, and its link still works.
	urls, _ := body["uploaded_before_failure"].([]any)
	if len(urls) != 1 {
		t.Fatalf("uploaded_before_failure = %v, want the first file's URL", body["uploaded_before_failure"])
	}
	if status, bytes := h.get(urls[0].(string)); status != 200 || bytes != "fake png bytes for the test" {
		t.Errorf("GET %v = %d %q", urls[0], status, bytes)
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

// Opening a run is not idempotent: a run committed under a lost response would
// be duplicated by a retry, and the first slug orphaned. So it gets one attempt.
func TestCreateRunIsNotRetried(t *testing.T) {
	var mu sync.Mutex
	var runPosts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/runs" {
			mu.Lock()
			runPosts++
			mu.Unlock()
		}
		// Retryable by policy — which is exactly why this endpoint must opt out.
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
	if runPosts != 1 {
		t.Errorf("POST /v1/runs was sent %d times, want exactly 1", runPosts)
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
	req, err := http.NewRequest(http.MethodPut, a.URL+"?upload_token=anything", strings.NewReader("swapped"))
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
	if _, body := h.get(a.URL); body != "fake png bytes for the test" {
		t.Errorf("stored bytes changed to %q", body)
	}
}
