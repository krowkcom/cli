package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krowkcom/cli/internal/api"
	"github.com/krowkcom/cli/internal/registry"
)

// session drives the server the way a client would: newline-delimited JSON-RPC
// in, newline-delimited JSON-RPC out.
type session struct {
	t       *testing.T
	server  *Server
	fixture string
}

func newSession(t *testing.T, token string) *session {
	t.Helper()

	srv := httptest.NewServer(registry.Handler(0, ""))
	t.Cleanup(srv.Close)

	fixture := filepath.Join(t.TempDir(), "checkout-after.png")
	if err := os.WriteFile(fixture, []byte("fake png bytes for the test"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := map[string]string{"KROWK_API_URL": srv.URL + "/v1"}

	return &session{
		t: t,
		server: &Server{
			Client: api.New(srv.URL+"/v1", token),
			Env:    func(k string) string { return env[k] },
			// The fixture's own directory, standing in for a checkout. Uploads
			// are confined to it, so tests exercise the same boundary a real
			// agent hits rather than an unconfined filesystem.
			Root:    filepath.Dir(fixture),
			Version: "1.2.3",
			Now:     func() time.Time { return time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC) },
		},
		fixture: fixture,
	}
}

// exchange sends the given messages and returns one decoded reply per response.
func (s *session) exchange(messages ...string) []map[string]any {
	s.t.Helper()

	var out bytes.Buffer
	in := strings.NewReader(strings.Join(messages, "\n") + "\n")
	if err := s.server.Serve(context.Background(), in, &out); err != nil {
		s.t.Fatalf("serve: %v", err)
	}

	var replies []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var reply map[string]any
		if err := json.Unmarshal([]byte(line), &reply); err != nil {
			s.t.Fatalf("reply is not JSON: %v\n%s", err, line)
		}
		replies = append(replies, reply)
	}
	return replies
}

// callTool runs one tools/call and returns its result object.
func (s *session) callTool(name string, args map[string]any) map[string]any {
	s.t.Helper()

	params, err := json.Marshal(map[string]any{"name": name, "arguments": args})
	if err != nil {
		s.t.Fatal(err)
	}
	req, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": json.RawMessage(params),
	})
	if err != nil {
		s.t.Fatal(err)
	}

	replies := s.exchange(string(req))
	if len(replies) != 1 {
		s.t.Fatalf("got %d replies, want 1: %+v", len(replies), replies)
	}
	if e, ok := replies[0]["error"]; ok {
		s.t.Fatalf("protocol error: %v", e)
	}
	result, _ := replies[0]["result"].(map[string]any)
	if result == nil {
		s.t.Fatalf("no result: %+v", replies[0])
	}
	return result
}

// text is the agent-facing text of a tool result.
func text(t *testing.T, result map[string]any) string {
	t.Helper()
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("no content: %+v", result)
	}
	first, _ := content[0].(map[string]any)
	s, _ := first["text"].(string)
	return s
}

func TestInitializeNegotiatesAndAdvertisesTools(t *testing.T) {
	s := newSession(t, "krk_test")

	replies := s.exchange(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","clientInfo":{"name":"probe","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	)
	// The notification in the middle must not produce a reply.
	if len(replies) != 2 {
		t.Fatalf("got %d replies, want 2 — a notification takes none: %+v", len(replies), replies)
	}

	init, _ := replies[0]["result"].(map[string]any)
	if init["protocolVersion"] != "2024-11-05" {
		t.Errorf("protocolVersion = %v, want the revision the client asked for", init["protocolVersion"])
	}
	info, _ := init["serverInfo"].(map[string]any)
	if info["name"] != "krowk" || info["version"] != "1.2.3" {
		t.Errorf("serverInfo = %+v", info)
	}
	// The instructions are where the paste rules live; a schema cannot say it.
	if instr, _ := init["instructions"].(string); !strings.Contains(instr, "claim_token") {
		t.Errorf("instructions should warn about the claim token, got %q", instr)
	}

	list, _ := replies[1]["result"].(map[string]any)
	advertised, _ := list["tools"].([]any)
	names := map[string]bool{}
	for _, tool := range advertised {
		m, _ := tool.(map[string]any)
		name, _ := m["name"].(string)
		names[name] = true
		if _, ok := m["inputSchema"].(map[string]any); !ok {
			t.Errorf("%s has no inputSchema", name)
		}
		if desc, _ := m["description"].(string); desc == "" {
			t.Errorf("%s has no description", name)
		}
	}
	for _, want := range []string{"krowk_push", "krowk_list_artifacts", "krowk_get_artifact",
		"krowk_claim_artifact", "krowk_get_run", "krowk_verify_key"} {
		if !names[want] {
			t.Errorf("%s is not advertised", want)
		}
	}
}

func TestUnknownProtocolFallsBackToOurNewest(t *testing.T) {
	s := newSession(t, "")
	replies := s.exchange(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)

	init, _ := replies[0]["result"].(map[string]any)
	if init["protocolVersion"] != protocols[0] {
		t.Errorf("protocolVersion = %v, want %q", init["protocolVersion"], protocols[0])
	}
}

func TestPushReturnsBothPasteFormsLabelled(t *testing.T) {
	s := newSession(t, "krk_test")

	result := s.callTool("krowk_push", map[string]any{
		"files":        []string{s.fixture},
		"title":        "Checkout — mobile",
		"pull_request": "https://github.com/acme/storefront/pull/412",
		"metadata":     map[string]any{"krowk.caption": "Checkout on mobile"},
	})
	if result["isError"] == true {
		t.Fatalf("push failed: %s", text(t, result))
	}

	body := text(t, result)
	// The caption is the artifact's own, so it is what the block says. A title
	// names the work and stays on the run.
	for _, want := range []string{"Paste into GitHub", "Paste into Slack", "![Checkout on mobile]("} {
		if !strings.Contains(body, want) {
			t.Errorf("text is missing %q:\n%s", want, body)
		}
	}

	structured, _ := result["structuredContent"].(map[string]any)
	pastes, _ := structured["pastes"].([]any)
	if len(pastes) != 1 {
		t.Fatalf("pastes = %+v, want one per artifact", structured["pastes"])
	}
	paste, _ := pastes[0].(map[string]any)
	markdown, _ := paste["markdown"].(string)
	url, _ := paste["url"].(string)
	// The embed names the bytes and is wrapped in a link to the card page, so
	// it starts with the wrapping link rather than with the bang.
	if !strings.HasPrefix(markdown, "[![Checkout on mobile](") {
		t.Errorf("paste.markdown = %q", markdown)
	}
	if url == "" || strings.Contains(url, "![") {
		t.Errorf("paste.url = %q, want the bare link", url)
	}
	// Metadata lives on the run, and the run must ride along in the structured
	// output — it is what makes the link worth having.
	run, _ := structured["run"].(map[string]any)
	meta, _ := run["metadata"].(map[string]any)
	if meta["krowk.change.url"] != "https://github.com/acme/storefront/pull/412" {
		t.Errorf("run metadata = %v, want the pull request attached", meta)
	}
	if meta["vcs.change.id"] != "412" {
		t.Errorf("vcs.change.id = %v, want it derived from the pull request URL", meta["vcs.change.id"])
	}
	if meta["krowk.client"] != "krowk-mcp/1.2.3" {
		t.Errorf("krowk.client = %v, want the MCP server to identify itself", meta["krowk.client"])
	}
}

// The push tool takes the structured links the CLI's --link writes, so an agent
// driving krowk through MCP records the same vocabulary rather than a flat list.
func TestPushRecordsStructuredLinksOnTheRun(t *testing.T) {
	s := newSession(t, "krk_test")

	result := s.callTool("krowk_push", map[string]any{
		"files": []string{s.fixture},
		"links": []any{
			map[string]any{
				"url":   "https://linear.app/acme/issue/ENG-9",
				"title": "Cart total rounds wrong",
				"rel":   "fixes",
			},
			map[string]any{"url": "https://github.com/acme/storefront/discussions/7"},
		},
	})
	if result["isError"] == true {
		t.Fatalf("push failed: %s", text(t, result))
	}

	structured, _ := result["structuredContent"].(map[string]any)
	run, _ := structured["run"].(map[string]any)
	meta, _ := run["metadata"].(map[string]any)
	links, _ := meta["krowk.links"].([]any)
	if len(links) != 2 {
		t.Fatalf("krowk.links = %v, want both entries", meta["krowk.links"])
	}
	first, _ := links[0].(map[string]any)
	if first["url"] != "https://linear.app/acme/issue/ENG-9" ||
		first["title"] != "Cart total rounds wrong" || first["rel"] != "fixes" {
		t.Errorf("krowk.links[0] = %v", first)
	}
	// A link with only a URL records only a URL: no empty title or rel keys, the
	// same pruning every other optional key gets.
	second, _ := links[1].(map[string]any)
	if len(second) != 1 || second["url"] != "https://github.com/acme/storefront/discussions/7" {
		t.Errorf("krowk.links[1] = %v, want the URL alone", second)
	}
}

// The same limits the CLI flag enforces, enforced here — and enforced before a
// single byte moves, since the point is to keep a malformed link out of a record
// nothing downstream validates.
func TestPushRefusesMalformedLinks(t *testing.T) {
	for name, links := range map[string][]any{
		"not a URL":     {map[string]any{"url": "ENG-9"}},
		"no URL at all": {map[string]any{"title": "an issue somewhere"}},
		"a title of two lines": {map[string]any{
			"url": "https://example.com/1", "title": "first\nsecond"}},
		"an overlong title": {map[string]any{
			"url": "https://example.com/1", "title": strings.Repeat("x", 141)}},
	} {
		t.Run(name, func(t *testing.T) {
			s := newSession(t, "krk_test")
			result := s.callTool("krowk_push", map[string]any{
				"files": []string{s.fixture},
				"links": links,
			})
			if result["isError"] != true {
				t.Fatalf("push succeeded with %v: %s", links, text(t, result))
			}
			if body := text(t, result); !strings.Contains(body, "links") {
				t.Errorf("text = %q, want it to name what was wrong", body)
			}
		})
	}
}

// Twenty-one links is a loop appending one per iteration, and it would fill the
// 16KB metadata cap with links while pushing the detected metadata out.
// A shape error has to name the argument it is about. The one message this used
// to carry blamed `files` for everything, which is advice to change the
// argument that was right.
func TestPushNamesTheArgumentThatIsTheWrongShape(t *testing.T) {
	s := newSession(t, "krk_test")

	result := s.callTool("krowk_push", map[string]any{
		"files": []string{s.fixture},
		// The likely mistake now that the schema has links: an array of URLs
		// rather than an array of link objects.
		"links": []any{"https://linear.app/acme/issue/ENG-9"},
	})
	if result["isError"] != true {
		t.Fatalf("push accepted a links array of strings: %s", text(t, result))
	}
	body := text(t, result)
	if !strings.Contains(body, "links") {
		t.Errorf("text = %q, want it to name `links`", body)
	}
	if strings.Contains(body, "`files` must be an array of paths") {
		t.Errorf("text = %q, want it to stop blaming files", body)
	}
}

// The schema says additionalProperties: false, so the tool enforces it. An
// agent writing `label` where the schema says `title` would otherwise get a
// link with no name on it and no word said about why.
// Same honesty in the tool: metadata about the work has nowhere to land on a
// run the caller named, and an agent told nothing would report the issue as
// linked when it is not.
func TestPushSaysWhenANamedRunAlreadyHasItsMetadata(t *testing.T) {
	s := newSession(t, "krk_test")

	first := s.callTool("krowk_push", map[string]any{"files": []string{s.fixture}})
	structured, _ := first["structuredContent"].(map[string]any)
	run, _ := structured["run"].(map[string]any)
	slug, _ := run["slug"].(string)
	if slug == "" {
		t.Fatalf("no run slug to reuse: %+v", structured)
	}

	result := s.callTool("krowk_push", map[string]any{
		"files": []string{s.fixture},
		"run":   slug,
		"links": []any{map[string]any{"url": "https://linear.app/acme/issue/ENG-9"}},
	})
	if result["isError"] == true {
		t.Fatalf("push failed: %s", text(t, result))
	}
	if body := text(t, result); !strings.Contains(body, "links") || !strings.Contains(body, slug) {
		t.Errorf("text = %q, want it to say the links were not recorded on %s", body, slug)
	}
}

func TestPushRefusesAnUnknownArgument(t *testing.T) {
	s := newSession(t, "krk_test")

	result := s.callTool("krowk_push", map[string]any{
		"files": []string{s.fixture},
		"links": []any{map[string]any{"url": "https://example.com/1", "label": "My Issue"}},
	})
	if result["isError"] != true {
		t.Fatalf("push accepted an unknown field: %s", text(t, result))
	}
	if body := text(t, result); !strings.Contains(body, "label") {
		t.Errorf("text = %q, want it to name the field it did not know", body)
	}
}

func TestPushRefusesMoreLinksThanARunHolds(t *testing.T) {
	s := newSession(t, "krk_test")

	links := make([]any, 0, 21)
	for i := 0; i < 21; i++ {
		links = append(links, map[string]any{"url": fmt.Sprintf("https://example.com/%d", i)})
	}
	result := s.callTool("krowk_push", map[string]any{
		"files": []string{s.fixture},
		"links": links,
	})
	if result["isError"] != true {
		t.Fatalf("push accepted 21 links: %s", text(t, result))
	}
}

func TestAnonymousPushWarnsAboutTheClaimToken(t *testing.T) {
	s := newSession(t, "") // no key

	result := s.callTool("krowk_push", map[string]any{"files": []string{s.fixture}})
	if result["isError"] == true {
		t.Fatalf("push failed: %s", text(t, result))
	}

	body := text(t, result)
	if !strings.Contains(body, "anonymous") || !strings.Contains(body, "do not paste it anywhere public") {
		t.Errorf("an anonymous push should warn about the claim token:\n%s", body)
	}

	structured, _ := result["structuredContent"].(map[string]any)
	artifacts, _ := structured["artifacts"].([]any)
	if len(artifacts) != 1 {
		t.Fatalf("artifacts = %+v, want one", structured["artifacts"])
	}
	artifact, _ := artifacts[0].(map[string]any)
	claim, _ := artifact["claim_token"].(string)
	if claim == "" {
		t.Fatal("no claim token on an anonymous push")
	}
	// It is a capability, so it must not be in either paste form.
	pastes, _ := structured["pastes"].([]any)
	paste, _ := pastes[0].(map[string]any)
	for k, v := range paste {
		if s, _ := v.(string); strings.Contains(s, claim) {
			t.Errorf("the claim token leaked into paste.%s: %q", k, s)
		}
	}
}

// A failed tool reports itself in the result, so the agent can read the fix
// rather than being handed a transport error it cannot act on.
func TestAFailedToolIsAResultNotAProtocolError(t *testing.T) {
	s := newSession(t, "krk_test")

	result := s.callTool("krowk_push", map[string]any{"files": []string{"/nope/absent.png"}})
	if result["isError"] != true {
		t.Fatalf("want isError, got %+v", result)
	}
	body := text(t, result)
	if !strings.Contains(body, "file_unreadable") || !strings.Contains(body, "fix:") {
		t.Errorf("text should name the failure and its fix:\n%s", body)
	}
	payload, _ := result["structuredContent"].(map[string]any)
	if payload["error"] != "file_unreadable" {
		t.Errorf("structuredContent = %+v, want the machine-readable body", payload)
	}
}

// An artifact is readable by anyone holding the link, and a model picks the
// paths — so a instruction hidden in a repository file or a fetched page must not
// be able to name a key and have it published.
func TestPushRefusesFilesOutsideTheRoot(t *testing.T) {
	s := newSession(t, "krk_test")
	root := filepath.Dir(s.fixture)

	// A secret next door to the root, the shape of ~/.ssh/id_rsa.
	outside := filepath.Join(t.TempDir(), "id_rsa")
	if err := os.WriteFile(outside, []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		outside,
		filepath.Join(root, "..", filepath.Base(filepath.Dir(outside)), "id_rsa"),
		"/etc/passwd",
	} {
		result := s.callTool("krowk_push", map[string]any{"files": []string{path}})
		if result["isError"] != true {
			t.Errorf("%s was accepted", path)
			continue
		}
		if body := text(t, result); !strings.Contains(body, "outside_root") &&
			!strings.Contains(body, "file_unreadable") {
			t.Errorf("%s: text = %q", path, body)
		}
	}
}

// A symlink inside the root pointing out of it defeats a prefix check that runs
// before symlinks are resolved, so resolve first.
func TestPushRefusesASymlinkEscapingTheRoot(t *testing.T) {
	s := newSession(t, "krk_test")
	root := filepath.Dir(s.fixture)

	secret := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(secret, []byte(`{"token":"krk_live_secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "innocent-screenshot.png")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	result := s.callTool("krowk_push", map[string]any{"files": []string{link}})
	if result["isError"] != true {
		t.Fatalf("a symlink out of the root was followed: %s", text(t, result))
	}
	if body := text(t, result); !strings.Contains(body, "outside_root") {
		t.Errorf("text = %q, want outside_root", body)
	}
}

// One bad path must not sneak through alongside a legitimate one.
func TestPushRefusesTheWholeBatchIfAnyFileIsOutside(t *testing.T) {
	s := newSession(t, "krk_test")

	outside := filepath.Join(t.TempDir(), "id_rsa")
	if err := os.WriteFile(outside, []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}

	result := s.callTool("krowk_push", map[string]any{"files": []string{s.fixture, outside}})
	if result["isError"] != true {
		t.Fatalf("a mixed batch was accepted: %s", text(t, result))
	}
}

// Confinement must not get in the way of the actual use case.
func TestPushAcceptsFilesInsideTheRoot(t *testing.T) {
	s := newSession(t, "krk_test")
	root := filepath.Dir(s.fixture)

	nested := filepath.Join(root, "screenshots")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(nested, "after.png")
	if err := os.WriteFile(deep, []byte("nested fake png"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Absolute, nested, and relative-with-a-detour all resolve inside.
	for _, path := range []string{deep, filepath.Join(root, "screenshots", "..", "screenshots", "after.png")} {
		result := s.callTool("krowk_push", map[string]any{"files": []string{path}})
		if result["isError"] == true {
			t.Errorf("%s was refused: %s", path, text(t, result))
		}
	}
}

// The root defaults to the working directory, so an agent started outside a
// checkout takes the home directory as its boundary — and then ~/.ssh/id_rsa and
// ~/.config/krowk/credentials.json are inside it. That is precisely the reach the
// confinement exists to remove, so a root that broad is refused instead.
func TestARootThatIsNoBoundaryIsRefused(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	for _, root := range []string{home, string(filepath.Separator)} {
		s := &Server{Root: root}
		if _, err := s.resolveRoot(); err == nil {
			t.Errorf("root %q was accepted", root)
		} else if code := err.(*api.Error).Code(); code != "root_too_broad" {
			t.Errorf("root %q: error = %q, want root_too_broad", root, code)
		}
	}

	// A root inside a secret-named directory is no boundary either: secretPath
	// only inspects components below the root, so a server started in ~/.ssh
	// would otherwise make config and authorized_keys pushable.
	ssh := filepath.Join(t.TempDir(), ".ssh")
	if err := os.Mkdir(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	s := &Server{Root: ssh}
	if _, err := s.resolveRoot(); err == nil {
		t.Errorf("root %q was accepted", ssh)
	} else if code := err.(*api.Error).Code(); code != "root_too_broad" {
		t.Errorf("root %q: error = %q, want root_too_broad", ssh, code)
	}

	// A project directory is the expected case and stays fine.
	if _, err := (&Server{Root: t.TempDir()}).resolveRoot(); err != nil {
		t.Errorf("a project directory was refused: %v", err)
	}
}

// A correct root is still not free of credentials: checkouts carry .env files,
// stray keys and service-account JSON, and all of them are inside the boundary.
func TestPushRefusesCredentialFilesInsideTheRoot(t *testing.T) {
	s := newSession(t, "krk_test")
	root := filepath.Dir(s.fixture)

	for _, rel := range []string{
		".env",
		".env.production",
		".netrc",
		"credentials.json",
		filepath.Join(".ssh", "id_rsa"),
		filepath.Join(".aws", "credentials"),
		filepath.Join("config", "id_ed25519"),
	} {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("SECRET"), 0o600); err != nil {
			t.Fatal(err)
		}

		result := s.callTool("krowk_push", map[string]any{"files": []string{path}})
		if result["isError"] != true {
			t.Errorf("%s was accepted for publishing", rel)
			continue
		}
		if body := text(t, result); !strings.Contains(body, "secret_path") {
			t.Errorf("%s: text = %q, want secret_path", rel, body)
		}
	}

	// The actual use case is untouched.
	result := s.callTool("krowk_push", map[string]any{"files": []string{s.fixture}})
	if result["isError"] == true {
		t.Errorf("a screenshot was refused: %s", text(t, result))
	}
}

// A hard link is a second name for one inode, not a link to anything — so a path
// inside the root can be a file outside it with nothing for EvalSymlinks to
// resolve. Nothing reports where the other names are, so a file with any is
// refused.
func TestPushRefusesAHardLinkedFile(t *testing.T) {
	s := newSession(t, "krk_test")
	root := filepath.Dir(s.fixture)

	outside := filepath.Join(t.TempDir(), "id_rsa")
	if err := os.WriteFile(outside, []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Same filesystem is a hard link's requirement; t.TempDir may not share one.
	link := filepath.Join(root, "screenshot-2.png")
	if err := os.Link(outside, link); err != nil {
		t.Skipf("hard links unavailable across these directories: %v", err)
	}

	result := s.callTool("krowk_push", map[string]any{"files": []string{link}})
	if result["isError"] != true {
		t.Fatalf("a hard link to a file outside the root was published: %s", text(t, result))
	}
	if body := text(t, result); !strings.Contains(body, "hard_linked") {
		t.Errorf("text = %q, want hard_linked", body)
	}
}

func TestPushNeedsFiles(t *testing.T) {
	s := newSession(t, "krk_test")

	result := s.callTool("krowk_push", map[string]any{})
	if result["isError"] != true {
		t.Fatalf("want isError, got %+v", result)
	}
	if body := text(t, result); !strings.Contains(body, "no_file") {
		t.Errorf("text = %q, want no_file", body)
	}
}

func TestGetArtifactRoundTripsAPush(t *testing.T) {
	s := newSession(t, "krk_test")

	pushed := s.callTool("krowk_push", map[string]any{"files": []string{s.fixture}})
	structured, _ := pushed["structuredContent"].(map[string]any)
	artifacts, _ := structured["artifacts"].([]any)
	if len(artifacts) != 1 {
		t.Fatalf("artifacts = %+v, want one", structured["artifacts"])
	}
	artifact, _ := artifacts[0].(map[string]any)
	slug, _ := artifact["slug"].(string)
	if slug == "" {
		t.Fatalf("no slug from the push: %+v", pushed)
	}

	got := s.callTool("krowk_get_artifact", map[string]any{"slug": slug})
	if got["isError"] == true {
		t.Fatalf("lookup failed: %s", text(t, got))
	}
	if body := text(t, got); !strings.Contains(body, "Artifact "+slug) {
		t.Errorf("text = %q, want the artifact slug", body)
	}
}

// An agent holding a link and not a slug is the ordinary case — the link is what
// a push hands back and what everything downstream shows. So `slug` takes one.
func TestGetArtifactTakesTheLinkAsReadilyAsTheSlug(t *testing.T) {
	s := newSession(t, "krk_test")

	pushed := s.callTool("krowk_push", map[string]any{"files": []string{s.fixture}})
	structured, _ := pushed["structuredContent"].(map[string]any)
	artifacts, _ := structured["artifacts"].([]any)
	if len(artifacts) != 1 {
		t.Fatalf("artifacts = %+v, want one", structured["artifacts"])
	}
	artifact, _ := artifacts[0].(map[string]any)
	slug, _ := artifact["slug"].(string)
	url, _ := artifact["url"].(string)
	if slug == "" || url == "" {
		t.Fatalf("no slug or url from the push: %+v", pushed)
	}

	got := s.callTool("krowk_get_artifact", map[string]any{"slug": url})
	if got["isError"] == true {
		t.Fatalf("lookup by link failed: %s", text(t, got))
	}
	if body := text(t, got); !strings.Contains(body, "Artifact "+slug) {
		t.Errorf("text = %q, want the artifact the link names", body)
	}

	// A link naming nothing is refused here rather than sent on as a slug, which
	// the registry could only answer as a record it does not have.
	refused := s.callTool("krowk_get_artifact", map[string]any{"slug": "https://krowk.com/pricing"})
	if refused["isError"] != true {
		t.Fatalf("want isError, got %+v", refused)
	}
	if body := text(t, refused); !strings.Contains(body, "bad_artifact") {
		t.Errorf("text = %q, want bad_artifact", body)
	}
}

func TestGetArtifactNeedsASlug(t *testing.T) {
	s := newSession(t, "krk_test")

	result := s.callTool("krowk_get_artifact", map[string]any{"slug": "  "})
	if result["isError"] != true {
		t.Fatalf("want isError, got %+v", result)
	}
	if body := text(t, result); !strings.Contains(body, "missing_slug") {
		t.Errorf("text = %q, want missing_slug", body)
	}
}

func TestGetArtifactSaysWhenThereIsNothingThere(t *testing.T) {
	s := newSession(t, "krk_test")

	result := s.callTool("krowk_get_artifact", map[string]any{"slug": "art_nosuchslug"})
	if result["isError"] != true {
		t.Fatalf("want isError, got %+v", result)
	}
	if body := text(t, result); !strings.Contains(body, "not_found") {
		t.Errorf("text = %q, want not_found", body)
	}
}

func TestGetRunReportsDetectedMetadata(t *testing.T) {
	s := newSession(t, "krk_test")

	result := s.callTool("krowk_get_run", nil)
	if result["isError"] == true {
		t.Fatalf("failed: %s", text(t, result))
	}
	// This test runs inside the CLI's own checkout, so detection has something.
	if body := text(t, result); !strings.Contains(body, "repo") {
		t.Errorf("text = %q, want the detected repository", body)
	}
	structured, _ := result["structuredContent"].(map[string]any)
	if structured["vcs.ref.head.revision"] == nil {
		t.Errorf("structuredContent = %+v, want the detected commit", structured)
	}
}

func TestVerifyKeyReportsTheWorkspaceAndAnonymousMode(t *testing.T) {
	s := newSession(t, "krk_test")

	result := s.callTool("krowk_verify_key", nil)
	if result["isError"] == true {
		t.Fatalf("failed: %s", text(t, result))
	}
	// An agent needs to know where its uploads land, which is the workspace.
	if body := text(t, result); !strings.Contains(body, "workspace") {
		t.Errorf("text = %q, want the workspace", body)
	}

	// Without a key the answer is "anonymous", not an error — pushing still works.
	anon := newSession(t, "")
	result = anon.callTool("krowk_verify_key", nil)
	if result["isError"] == true {
		t.Fatalf("no key should not be a failure: %s", text(t, result))
	}
	if body := text(t, result); !strings.Contains(body, "anonymous") {
		t.Errorf("text = %q, want it to explain anonymous mode", body)
	}
}

// An anonymous upload is kept by spending its claim token, and afterwards it
// shows up in the key's own workspace listing.
func TestClaimAdoptsAnAnonymousPushIntoTheWorkspace(t *testing.T) {
	anon := newSession(t, "")

	slug, claim := anon.anonymousPush()

	// Same registry, now with a key.
	keyed := anon.withKey("krowk_sk_test")

	claimed := keyed.callTool("krowk_claim_artifact", map[string]any{
		"slug": slug, "claim_token": claim,
	})
	if claimed["isError"] == true {
		t.Fatalf("claim failed: %s", text(t, claimed))
	}
	// A claim with no run leaves the upload under nothing, and this tool is the
	// only way it ever gets one — the same gap the CLI closes with a breadcrumb.
	// There are no breadcrumbs here, so it has to be in the text.
	if body := text(t, claimed); !strings.Contains(body, "It belongs to no run") {
		t.Errorf("a claim with no run says nothing about grouping it:\n%s", body)
	}

	listed := keyed.callTool("krowk_list_artifacts", nil)
	if listed["isError"] == true {
		t.Fatalf("list failed: %s", text(t, listed))
	}
	if body := text(t, listed); !strings.Contains(body, slug) {
		t.Errorf("listing = %q, want the claimed artifact in the workspace", body)
	}

	// Listing without a key is refused: keyless uploads share one workspace.
	unlisted := anon.callTool("krowk_list_artifacts", nil)
	if unlisted["isError"] != true {
		t.Fatalf("keyless list should fail, got %+v", unlisted)
	}
	if body := text(t, unlisted); !strings.Contains(body, "unauthorized") {
		t.Errorf("text = %q, want unauthorized", body)
	}
}

// The claim spends a one-shot token, so what it does with a link — and what it
// refuses before spending anything — is worth pinning on this transport too.
func TestClaimTakesLinksAndRefusesBeforeSpendingTheToken(t *testing.T) {
	anon := newSession(t, "")
	slug, claim := anon.anonymousPush()
	keyed := anon.withKey("krowk_sk_test")
	run := keyed.startRun()

	// A run argument that carries no run is refused, and the token survives it:
	// the claim below still works.
	refused := keyed.callTool("krowk_claim_artifact", map[string]any{
		"slug": slug, "claim_token": claim, "run": "https://krowk.com/pricing",
	})
	if refused["isError"] != true {
		t.Fatalf("a run link naming nothing was accepted: %+v", refused)
	}
	if body := text(t, refused); !strings.Contains(body, "bad_run") {
		t.Errorf("text = %q, want bad_run", body)
	}

	claimed := keyed.callTool("krowk_claim_artifact", map[string]any{
		"slug": "https://krowk.com/a/" + slug, "claim_token": claim, "run": run,
	})
	if claimed["isError"] == true {
		t.Fatalf("claim by link failed: %s", text(t, claimed))
	}
	if body := text(t, claimed); !strings.Contains(body, slug) || !strings.Contains(body, run) {
		t.Errorf("text = %q, want the artifact under the run", body)
	}
}

// The agent spending the claim token is the one that knows which run the upload
// came from, so claiming takes the run rather than a separate attach tool. An
// anonymous upload has no run and no other way to get one.
func TestClaimGroupsTheAdoptedUploadUnderARun(t *testing.T) {
	anon := newSession(t, "")
	slug, claim := anon.anonymousPush()

	keyed := anon.withKey("krowk_sk_test")
	run := keyed.startRun()

	claimed := keyed.callTool("krowk_claim_artifact", map[string]any{
		"slug": slug, "claim_token": claim, "run": run,
	})
	if claimed["isError"] == true {
		t.Fatalf("claim with a run failed: %s", text(t, claimed))
	}
	// And a claim that did name a run is not told to go and find one.
	if body := text(t, claimed); strings.Contains(body, "It belongs to no run") {
		t.Errorf("an upload already grouped was told it belongs to no run:\n%s", body)
	}
	if body := text(t, claimed); !strings.Contains(body, "Grouped under run "+run) {
		t.Errorf("text = %q, want the grouping named the way a push names it", body)
	}
	structured, _ := claimed["structuredContent"].(map[string]any)
	artifact, _ := structured["artifact"].(map[string]any)
	if runSlugOf(artifact) != run {
		t.Errorf("artifact run = %v, want %q", artifact["run"], run)
	}
}

// The claim is spent before the attach is tried, so a failing attach must not
// read as a claim the agent can simply repeat.
func TestAFailedAttachSaysTheClaimStillLanded(t *testing.T) {
	anon := newSession(t, "")
	slug, claim := anon.anonymousPush()
	keyed := anon.withKey("krowk_sk_test")

	failed := keyed.callTool("krowk_claim_artifact", map[string]any{
		"slug": slug, "claim_token": claim, "run": "run_nosuchrunatall00000",
	})
	if failed["isError"] != true {
		t.Fatalf("attaching to an unknown run should fail: %+v", failed)
	}
	body := text(t, failed)
	if !strings.Contains(body, "claimed and kept") {
		t.Errorf("text = %q, want it to say the claim landed", body)
	}
	// The retry has to be one this transport can make. An agent here cannot shell
	// out, so naming the CLI's `uploads attach` would be advice it cannot follow.
	if !strings.Contains(body, "krowk_claim_artifact again") {
		t.Errorf("text = %q, want the retry to be a tool call", body)
	}
	if strings.Contains(body, "uploads attach") {
		t.Errorf("text = %q, want no shell command on the MCP surface", body)
	}
	structured, _ := failed["structuredContent"].(map[string]any)
	if structured["claimed"] != slug {
		t.Errorf("claimed = %v, want %q", structured["claimed"], slug)
	}

	// And the advice works: the same slug and token, with a run that exists, is the
	// same success — which is what makes re-calling the tool the retry.
	run := keyed.startRun()
	retried := keyed.callTool("krowk_claim_artifact", map[string]any{
		"slug": slug, "claim_token": claim, "run": run,
	})
	if retried["isError"] == true {
		t.Fatalf("the advertised retry failed: %s", text(t, retried))
	}
	structured, _ = retried["structuredContent"].(map[string]any)
	artifact, _ := structured["artifact"].(map[string]any)
	if runSlugOf(artifact) != run {
		t.Errorf("retry left run = %v, want %q", artifact["run"], run)
	}
}

// A server whose checkout pinned a workspace it could not resolve serves on, but
// nothing it creates may land in the anonymous workspace instead: that is the
// silent misdirection — a link that works, pointing at the wrong place.
func TestAnUnresolvedWorkspaceRefusesUploadsAndStillReads(t *testing.T) {
	s := newSession(t, "")

	// Something to read back later, uploaded while the server still could.
	slug, _ := s.anonymousPush()

	s.server.WorkspaceErr = api.Fail("no_stored_key",
		"run `krowk auth login`, or `krowk workspaces` to see what is stored")

	for _, tool := range []struct {
		name string
		args map[string]any
	}{
		{"krowk_push", map[string]any{"files": []string{s.fixture}}},
		{"krowk_list_artifacts", nil},
		{"krowk_claim_artifact", map[string]any{"slug": slug, "claim_token": "krowk_claim_whatever"}},
	} {
		result := s.callTool(tool.name, tool.args)
		if result["isError"] != true {
			t.Fatalf("%s should be refused, got %+v", tool.name, result)
		}
		body := text(t, result)
		if !strings.Contains(body, "no_stored_key") || !strings.Contains(body, "krowk auth login") {
			t.Errorf("%s text = %q, want the workspace reason and its fix", tool.name, body)
		}
	}

	// Reading needs no credential, so it is unaffected.
	got := s.callTool("krowk_get_artifact", map[string]any{"slug": slug})
	if got["isError"] == true {
		t.Fatalf("reading an artifact should still work: %s", text(t, got))
	}
	if body := text(t, got); !strings.Contains(body, "Artifact "+slug) {
		t.Errorf("text = %q, want the artifact", body)
	}
	if run := s.callTool("krowk_get_run", nil); run["isError"] == true {
		t.Fatalf("run context should still work: %s", text(t, run))
	}
}

// And the agent can find out why, rather than guessing from a refusal.
func TestVerifyKeyReportsAnUnresolvedWorkspace(t *testing.T) {
	s := newSession(t, "")
	s.server.WorkspaceErr = api.Fail("config_unreadable", "fix `.krowk/config.json` and restart the server")

	result := s.callTool("krowk_verify_key", nil)
	// A status tool reports the state it is in; it does not fail because of it.
	if result["isError"] == true {
		t.Fatalf("verify_key should report, not fail: %s", text(t, result))
	}
	body := text(t, result)
	if !strings.Contains(body, "config_unreadable") || !strings.Contains(body, ".krowk/config.json") {
		t.Errorf("text = %q, want the reason and its fix", body)
	}
	structured, _ := result["structuredContent"].(map[string]any)
	if structured["authenticated"] != false {
		t.Errorf("authenticated = %v, want false", structured["authenticated"])
	}
	if got, _ := structured["workspace_error"].(string); !strings.Contains(got, "config_unreadable") {
		t.Errorf("workspace_error = %q, want the reason", got)
	}
}

// anonymousPush uploads the fixture without a key and returns the slug and the
// claim token it came back with.
func (s *session) anonymousPush() (slug, claimToken string) {
	s.t.Helper()
	pushed := s.callTool("krowk_push", map[string]any{"files": []string{s.fixture}})
	structured, _ := pushed["structuredContent"].(map[string]any)
	artifacts, _ := structured["artifacts"].([]any)
	if len(artifacts) == 0 {
		s.t.Fatalf("push returned nothing: %+v", pushed)
	}
	artifact, _ := artifacts[0].(map[string]any)
	slug, _ = artifact["slug"].(string)
	claimToken, _ = artifact["claim_token"].(string)
	if slug == "" || claimToken == "" {
		s.t.Fatalf("anonymous push returned no claimable artifact: %+v", artifact)
	}
	return slug, claimToken
}

// withKey is the same registry seen through a keyed client, which is how the
// claim flow's two halves are exercised in one test.
func (s *session) withKey(token string) *session {
	return &session{t: s.t, server: &Server{
		Client:  api.New(s.server.Client.BaseURL, token),
		Env:     s.server.Env,
		Root:    s.server.Root,
		Version: s.server.Version,
		Now:     s.server.Now,
	}, fixture: s.fixture}
}

// startRun opens a run to attach to. There is no tool for it — runs are the
// CLI's business — so it goes through the client the server holds.
func (s *session) startRun() string {
	s.t.Helper()
	run, err := s.server.Client.CreateRun(context.Background(), map[string]any{"agent": "test"})
	if err != nil {
		s.t.Fatalf("create run: %v", err)
	}
	return run.Slug
}

func TestUnknownToolAndMethodAreProtocolErrors(t *testing.T) {
	s := newSession(t, "krk_test")

	replies := s.exchange(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"krowk_yeet","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"resources/list"}`,
	)
	if len(replies) != 2 {
		t.Fatalf("got %d replies, want 2", len(replies))
	}
	for i, reply := range replies {
		e, _ := reply["error"].(map[string]any)
		if e == nil {
			t.Fatalf("reply %d should be an error: %+v", i, reply)
		}
		if _, ok := reply["result"]; ok {
			t.Errorf("reply %d carries both a result and an error", i)
		}
	}
}

func TestMalformedLineDoesNotKillTheSession(t *testing.T) {
	s := newSession(t, "krk_test")

	replies := s.exchange(
		`this is not json`,
		``,
		`{"jsonrpc":"2.0","id":7,"method":"ping"}`,
	)
	if len(replies) != 2 {
		t.Fatalf("got %d replies, want a parse error then the pong: %+v", len(replies), replies)
	}
	if e, _ := replies[0]["error"].(map[string]any); e == nil || e["code"] != float64(-32700) {
		t.Errorf("first reply = %+v, want a -32700 parse error", replies[0])
	}
	if replies[1]["id"] != float64(7) {
		t.Errorf("second reply = %+v, want the ping answered", replies[1])
	}
}

// A line that is valid JSON but not a Request object — a batch array, a bare
// string — is an invalid request (-32600), not a parse error (-32700).
func TestValidJSONThatIsNotARequestIsInvalidNotUnparseable(t *testing.T) {
	s := newSession(t, "krk_test")

	replies := s.exchange(
		`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`,
		`"just a string"`,
		`null`,
		`{}`,
		`{"jsonrpc":"2.0","id":5}`,
		`{"jsonrpc":"2.0","id":7,"method":"ping"}`,
	)
	if len(replies) != 6 {
		t.Fatalf("got %d replies, want five invalid-request errors then the pong: %+v", len(replies), replies)
	}
	for i := range 5 {
		if e, _ := replies[i]["error"].(map[string]any); e == nil || e["code"] != float64(-32600) {
			t.Errorf("reply %d = %+v, want a -32600 invalid request", i, replies[i])
		}
	}
	if replies[5]["id"] != float64(7) {
		t.Errorf("last reply = %+v, want the ping answered", replies[5])
	}
}

// Cancelling the context must end the server even while it sits blocked in a
// read — that is what lets Ctrl-C kill the process.
func TestCancellationEndsAnIdleServer(t *testing.T) {
	s := newSession(t, "krk_test")

	in, _ := io.Pipe() // never written to, so the read blocks forever
	t.Cleanup(func() { in.Close() })
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- s.server.Serve(ctx, in, &bytes.Buffer{}) }()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve = %v, want a clean return on cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the context was cancelled")
	}
}

// One unterminated line over maxLine must end the session at the cap, not
// after the whole thing has been buffered.
func TestAMessageOverTheCapEndsTheSession(t *testing.T) {
	s := newSession(t, "krk_test")

	var out bytes.Buffer
	in := strings.NewReader(strings.Repeat("x", maxLine+1))
	err := s.server.Serve(context.Background(), in, &out)
	if err == nil || !strings.Contains(err.Error(), "longer than") {
		t.Fatalf("Serve = %v, want the message-too-long error", err)
	}
	if out.Len() != 0 {
		t.Errorf("stdout carried %q, want nothing", out.String())
	}
}

func TestMalformedInitializeParamsAreAProtocolError(t *testing.T) {
	s := newSession(t, "krk_test")

	replies := s.exchange(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":"not an object"}`)
	if len(replies) != 1 {
		t.Fatalf("got %d replies, want 1: %+v", len(replies), replies)
	}
	e, _ := replies[0]["error"].(map[string]any)
	if e == nil || e["code"] != float64(-32602) {
		t.Errorf("reply = %+v, want a -32602 invalid-params error", replies[0])
	}
}

// Nothing but protocol traffic may reach stdout, or the client cannot parse it.
func TestEveryLineOnStdoutIsAJSONRPCResponse(t *testing.T) {
	s := newSession(t, "")

	var out bytes.Buffer
	in := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"krowk_push","arguments":{"files":["/nope.png"]}}}`,
	}, "\n") + "\n")

	if err := s.server.Serve(context.Background(), in, &out); err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		var reply struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal([]byte(line), &reply); err != nil {
			t.Fatalf("stdout carried a non-JSON line: %q", line)
		}
		if reply.JSONRPC != "2.0" {
			t.Errorf("line is not JSON-RPC 2.0: %q", line)
		}
		if len(reply.ID) == 0 {
			t.Errorf("response has no id: %q", line)
		}
	}
}

// runSlugOf digs the slug out of the run an artifact reports. The run rides on
// an artifact as a nested object rather than as a bare slug, so a client
// reading one back learns what produced it without a second call.
func runSlugOf(artifact map[string]any) string {
	run, _ := artifact["run"].(map[string]any)
	slug, _ := run["slug"].(string)
	return slug
}
