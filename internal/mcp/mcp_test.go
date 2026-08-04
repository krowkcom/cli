package mcp

import (
	"bytes"
	"context"
	"encoding/json"
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
	if instr, _ := init["instructions"].(string); !strings.Contains(instr, "claim_url") {
		t.Errorf("instructions should warn about the claim URL, got %q", instr)
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
	for _, want := range []string{"krowk_push", "krowk_get_artifact", "krowk_get_run", "krowk_verify_key"} {
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
	})
	if result["isError"] == true {
		t.Fatalf("push failed: %s", text(t, result))
	}

	body := text(t, result)
	for _, want := range []string{"Paste into GitHub", "Paste into Slack", "[![Checkout — mobile]("} {
		if !strings.Contains(body, want) {
			t.Errorf("text is missing %q:\n%s", want, body)
		}
	}

	structured, _ := result["structuredContent"].(map[string]any)
	paste, _ := structured["paste"].(map[string]any)
	markdown, _ := paste["markdown"].(string)
	url, _ := paste["url"].(string)
	if !strings.HasPrefix(markdown, "[![Checkout — mobile](") {
		t.Errorf("paste.markdown = %q", markdown)
	}
	if !strings.Contains(url, "/a/") {
		t.Errorf("paste.url = %q", url)
	}
	// Structured output must carry the metadata that makes the link worth having.
	artifact, _ := structured["artifact"].(map[string]any)
	meta, _ := artifact["metadata"].(map[string]any)
	if meta["pull_request"] != "https://github.com/acme/storefront/pull/412" {
		t.Errorf("metadata = %v, want the pull request attached", meta)
	}
	if meta["client"] != "krowk-mcp/1.2.3" {
		t.Errorf("client = %v, want the MCP server to identify itself", meta["client"])
	}
}

func TestAnonymousPushWarnsAboutTheClaimURL(t *testing.T) {
	s := newSession(t, "") // no key

	result := s.callTool("krowk_push", map[string]any{"files": []string{s.fixture}})
	if result["isError"] == true {
		t.Fatalf("push failed: %s", text(t, result))
	}

	body := text(t, result)
	if !strings.Contains(body, "anonymous") || !strings.Contains(body, "do not paste it anywhere public") {
		t.Errorf("an anonymous push should warn about the claim URL:\n%s", body)
	}

	structured, _ := result["structuredContent"].(map[string]any)
	artifact, _ := structured["artifact"].(map[string]any)
	claim, _ := artifact["claim_url"].(string)
	if claim == "" {
		t.Fatal("no claim URL on an anonymous push")
	}
	// It is a capability, so it must not be in either paste form.
	paste, _ := structured["paste"].(map[string]any)
	for k, v := range paste {
		if s, _ := v.(string); strings.Contains(s, claim) {
			t.Errorf("the claim URL leaked into paste.%s: %q", k, s)
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
	artifact, _ := structured["artifact"].(map[string]any)
	id, _ := artifact["id"].(string)
	if id == "" {
		t.Fatalf("no ID from the push: %+v", pushed)
	}

	got := s.callTool("krowk_get_artifact", map[string]any{"id": id})
	if got["isError"] == true {
		t.Fatalf("lookup failed: %s", text(t, got))
	}
	if body := text(t, got); !strings.Contains(body, "Artifact "+id) {
		t.Errorf("text = %q, want the artifact ID", body)
	}
}

func TestGetArtifactSaysWhenThereIsNothingThere(t *testing.T) {
	s := newSession(t, "krk_test")

	result := s.callTool("krowk_get_artifact", map[string]any{"id": "nosuchid"})
	if result["isError"] != true {
		t.Fatalf("want isError, got %+v", result)
	}
	if body := text(t, result); !strings.Contains(body, "artifact_not_found") {
		t.Errorf("text = %q, want artifact_not_found", body)
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
	if structured["commit"] == nil {
		t.Errorf("structuredContent = %+v, want the detected commit", structured)
	}
}

func TestVerifyKeyReportsScopesAndAnonymousMode(t *testing.T) {
	s := newSession(t, "krk_test")

	result := s.callTool("krowk_verify_key", nil)
	if result["isError"] == true {
		t.Fatalf("failed: %s", text(t, result))
	}
	if body := text(t, result); !strings.Contains(body, "artifacts:write") {
		t.Errorf("text = %q, want the scopes", body)
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

func TestReadOnlyKeyFailsThePushWithTheRegistrysReason(t *testing.T) {
	s := newSession(t, "krk_ro_readonly")

	result := s.callTool("krowk_push", map[string]any{"files": []string{s.fixture}})
	if result["isError"] != true {
		t.Fatalf("want isError, got %+v", result)
	}
	if body := text(t, result); !strings.Contains(body, "insufficient_scope") {
		t.Errorf("text = %q, want insufficient_scope", body)
	}
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
