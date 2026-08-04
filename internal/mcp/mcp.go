// Package mcp exposes the same /v1 API the CLI speaks as MCP tools, so an agent
// that cannot shell out can still push an artifact and get a link back.
//
// It is a thin client and nothing more: no logic lives here that is not also in
// the CLI, and every tool result carries the paste-ready markdown and the bare
// URL, labelled, so the agent's job is copy-paste rather than templating.
//
// The transport is MCP over stdio: one JSON-RPC 2.0 message per line, requests
// on stdin, responses on stdout. Standard library only, like the rest of the
// binary.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/krowkcom/cli/internal/api"
	"github.com/krowkcom/cli/internal/output"
	"github.com/krowkcom/cli/internal/runctx"
)

// Protocol revisions this server understands, newest first.
var protocols = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// maxLine caps one JSON-RPC message. Bytes travel over presigned URLs, never
// through this transport, so a request is a filename list at worst.
const maxLine = 4 << 20

// instructions tell the agent what to do with what it gets back, which is the
// part a tool schema cannot express.
const instructions = `krowk turns local files into permalinks that unfurl with their run metadata attached.

Call krowk_push with the paths you want to share. It returns two paste-ready
forms and you must pick by destination:

  - markdown  ` + output.EmbedSurfaces + `
  - url       ` + output.LinkSurfaces + `

Pasting the markdown form into Slack shows raw text; pasting the bare URL into a
GitHub comment shows a plain link with no image. Neither surface renders the
other's form, so choose deliberately.

If the push comes back anonymous it carries a claim_url. That link adopts the
upload, so treat it as a secret: give it to the human, never paste it into a
pull request, an issue or a chat message.

krowk_push only uploads files from the working directory and below. Anything
outside it is refused, symlinks included, and credential files are refused even
inside it — .env, .ssh, .aws, .netrc, private keys, credentials.json. An artifact
is published at a URL that needs no credential to read, so do not try to route
around this: if a file you were asked to share sits elsewhere, say so and let the
human move it.`

// Server speaks MCP over a pair of streams.
type Server struct {
	// Client talks to the registry. Required.
	Client *api.Client
	// Env resolves environment lookups for run-context detection.
	Env runctx.Env
	// Version is reported to the client and recorded on every upload.
	Version string
	// Root confines which files may be uploaded. Paths are resolved, symlinks
	// and all, and anything landing outside is refused.
	//
	// This matters more here than in the CLI. There a person types the path; here
	// a model chooses it, and a model reads repository files, web pages and issue
	// bodies — so an instruction hidden in any of them would otherwise turn
	// krowk_push into "read any file on this machine and publish it at a URL I can
	// fetch without credentials". Empty means the working directory.
	Root string
	// Now is swapped out in tests so expiry text is stable.
	Now func() time.Time
}

// resolveRoot is the confinement boundary, fully resolved once at startup.
func (s *Server) resolveRoot() (string, error) {
	root := s.Root
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", api.Fail("no_working_directory", "cannot determine the working directory: "+err.Error())
		}
		root = wd
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", api.Fail("bad_root", "cannot resolve "+root+": "+err.Error())
	}
	// Resolve the root too: on macOS the working directory is often under
	// /var, which is a symlink to /private/var, and comparing a resolved path
	// against an unresolved root would reject everything.
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	// A root of / or the home directory is not a boundary. The home directory is
	// the one that happens by accident: an agent started outside a checkout takes
	// its working directory as the root, and then ~/.ssh/id_rsa, ~/.aws and
	// ~/.config/krowk are all inside it — exactly the reach confinement is here to
	// remove. Better to refuse and be told where the files are.
	if abs == string(filepath.Separator) {
		return "", api.Fail("root_too_broad",
			"the upload root is / — start the server in a project directory, or pass --root")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if resolved, err := filepath.EvalSymlinks(home); err == nil {
			home = resolved
		}
		if abs == home {
			return "", api.Fail("root_too_broad",
				"the upload root is the home directory, which holds ~/.ssh and ~/.aws — "+
					"start the server in a project directory, or pass --root")
		}
	}
	return abs, nil
}

// secretNames are refused wherever they sit, root or no root, because a
// repository is not free of credentials either: .env files live in checkouts, and
// so do stray keys and service-account JSON. An artifact is published at a URL
// that needs no credential to read, and nobody publishes these on purpose — so
// the cost of refusing them is a clear error, and the cost of not refusing them
// is a leaked secret with a permalink.
var secretNames = map[string]bool{
	".ssh": true, ".aws": true, ".gnupg": true, ".kube": true, ".docker": true,
	".env": true, ".netrc": true, ".npmrc": true, ".pypirc": true, ".git-credentials": true,
	"credentials.json": true, "id_rsa": true, "id_ed25519": true, "id_ecdsa": true,
}

// secretPath reports whether any part of the path is one of those names, so a
// directory match covers everything under it.
func secretPath(root, resolved string) string {
	rel, err := filepath.Rel(root, resolved)
	if err != nil {
		return ""
	}
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		lower := strings.ToLower(part)
		// .env.local and .env.production are the same file by another name.
		if secretNames[lower] || strings.HasPrefix(lower, ".env.") {
			return part
		}
	}
	return ""
}

// permit resolves one requested path and confirms it lies inside root.
func permit(root, path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", api.Fail("file_unreadable", "cannot resolve `"+path+"`: "+err.Error())
	}
	// Symlinks are resolved before the check, not after: a link inside the root
	// pointing at ~/.ssh/id_rsa would sail through a plain prefix test.
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", api.Fail("file_unreadable",
			"cannot read `"+path+"` — paths resolve from the working directory")
	}
	rel, err := filepath.Rel(root, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", api.Fail("outside_root",
			"`"+path+"` is outside "+root+" — krowk_push only uploads files from the working directory")
	}
	if name := secretPath(root, real); name != "" {
		return "", api.Fail("secret_path",
			"`"+path+"` is a credential file (`"+name+"`) — refusing to publish it at a public URL")
	}
	return real, nil
}

// Serve reads requests until the stream ends or the context is cancelled.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	if s.Now == nil {
		s.Now = time.Now
	}

	reader := bufio.NewReaderSize(in, 64<<10)
	writer := bufio.NewWriter(out)
	encoder := json.NewEncoder(writer)

	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		line, err := readLine(reader)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}

		var req request
		if json.Unmarshal(line, &req) != nil {
			// Nothing to correlate a reply to, so answer with a null id, which
			// is what JSON-RPC asks for on a parse failure.
			if err := write(encoder, writer, response{
				JSONRPC: "2.0",
				ID:      json.RawMessage("null"),
				Error:   &rpcError{Code: -32700, Message: "the message is not JSON"},
			}); err != nil {
				return err
			}
			continue
		}

		result, rpcErr := s.dispatch(ctx, req)

		// A notification has no id and takes no reply, even on error.
		if req.isNotification() {
			continue
		}
		res := response{JSONRPC: "2.0", ID: req.ID}
		if rpcErr != nil {
			res.Error = rpcErr
		} else {
			res.Result = result
		}
		if err := write(encoder, writer, res); err != nil {
			return err
		}
	}
}

func readLine(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadBytes('\n')
	if len(line) > maxLine {
		return nil, fmt.Errorf("mcp: message longer than %d bytes", maxLine)
	}
	if err == io.EOF && len(line) > 0 {
		return line, nil // a final line with no trailing newline
	}
	return line, err
}

func write(e *json.Encoder, w *bufio.Writer, res response) error {
	if err := e.Encode(res); err != nil {
		return err
	}
	return w.Flush()
}

func (s *Server) dispatch(ctx context.Context, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return s.initialize(req.Params), nil
	case "notifications/initialized", "notifications/cancelled":
		return nil, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": toolSchemas()}, nil
	case "tools/call":
		return s.call(ctx, req.Params)
	}
	return nil, &rpcError{Code: -32601, Message: "unknown method " + req.Method}
}

func (s *Server) initialize(params json.RawMessage) any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)

	return map[string]any{
		"protocolVersion": negotiate(p.ProtocolVersion),
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "krowk", "version": s.Version},
		"instructions":    instructions,
	}
}

// negotiate answers with the client's revision when it is one we speak, and
// with our newest otherwise, as the spec prescribes.
func negotiate(requested string) string {
	for _, v := range protocols {
		if v == requested {
			return v
		}
	}
	return protocols[0]
}

func (s *Server) call(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(params, &p) != nil {
		return nil, &rpcError{Code: -32602, Message: "tools/call needs a name and arguments"}
	}

	run, ok := tools[p.Name]
	if !ok {
		// An unknown tool is a protocol-level mistake, not a failed call.
		return nil, &rpcError{Code: -32602, Message: "unknown tool " + p.Name}
	}

	text, structured, err := run(ctx, s, p.Arguments)
	if err != nil {
		// A tool that fails reports it in the result, so the agent can read the
		// reason and fix the call instead of seeing a transport error.
		return toolResult(describeError(err), errorPayload(err), true), nil
	}
	return toolResult(text, structured, false), nil
}

func toolResult(text string, structured any, isError bool) map[string]any {
	result := map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
	if structured != nil {
		result["structuredContent"] = structured
	}
	return result
}

// describeError renders a failure the way the CLI does, fix and all, because
// that text is what the agent reads.
func describeError(err error) string {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		return "krowk failed: " + err.Error()
	}
	lines := []string{"krowk failed: " + apiErr.Code()}
	if apiErr.Status != 0 {
		lines[0] += fmt.Sprintf(" (HTTP %d)", apiErr.Status)
	}
	if fix := apiErr.Fix(); fix != "" {
		lines = append(lines, "fix: "+fix)
	}
	if apiErr.Retryable() {
		lines = append(lines, "this one is worth retrying")
	}
	return strings.Join(lines, "\n")
}

func errorPayload(err error) any {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		return map[string]any{"error": "cli_error", "detail": err.Error()}
	}
	return apiErr.Body
}

// ---- tools ----

// tool runs one call, returning the agent-facing text and the machine payload.
type tool func(ctx context.Context, s *Server, args json.RawMessage) (string, any, error)

var tools = map[string]tool{
	"krowk_push":         push,
	"krowk_get_artifact": getArtifact,
	"krowk_get_run":      getRun,
	"krowk_verify_key":   verifyKey,
}

func push(ctx context.Context, s *Server, args json.RawMessage) (string, any, error) {
	var a struct {
		Files       []string `json:"files"`
		Title       string   `json:"title"`
		PullRequest string   `json:"pull_request"`
		Reference   []string `json:"reference"`
		Session     string   `json:"session"`
		Repo        string   `json:"repo"`
		Commit      string   `json:"commit"`
		Agent       string   `json:"agent"`
	}
	if len(args) > 0 && json.Unmarshal(args, &a) != nil {
		return "", nil, api.Fail("bad_arguments", "`files` must be an array of paths")
	}
	if len(a.Files) == 0 {
		return "", nil, api.Fail("no_file", "pass at least one path in `files`")
	}

	// Confine before reading anything: an instruction the model picked up from a
	// repository file or a web page must not be able to name ~/.ssh/id_rsa here.
	root, err := s.resolveRoot()
	if err != nil {
		return "", nil, err
	}
	files := make([]string, 0, len(a.Files))
	for _, path := range a.Files {
		allowed, err := permit(root, path)
		if err != nil {
			return "", nil, err
		}
		files = append(files, allowed)
	}

	metadata := runctx.Resolve(s.Env, runctx.Overrides{
		Repo:        a.Repo,
		Commit:      a.Commit,
		Agent:       a.Agent,
		PullRequest: a.PullRequest,
		Reference:   a.Reference,
		Session:     a.Session,
		Title:       a.Title,
		Client:      "krowk-mcp/" + s.Version,
	})

	artifact, err := s.Client.CreateArtifact(ctx, files, metadata)
	if err != nil {
		return "", nil, err
	}
	return s.render(artifact, a.Title)
}

func getArtifact(ctx context.Context, s *Server, args json.RawMessage) (string, any, error) {
	var a struct {
		ID string `json:"id"`
	}
	if len(args) > 0 && json.Unmarshal(args, &a) != nil {
		return "", nil, api.Fail("bad_arguments", "`id` must be a string")
	}

	artifact, err := s.Client.GetArtifact(ctx, strings.TrimSpace(a.ID))
	if err != nil {
		return "", nil, err
	}
	return s.render(artifact, "")
}

func getRun(_ context.Context, s *Server, _ json.RawMessage) (string, any, error) {
	metadata := runctx.Resolve(s.Env, runctx.Overrides{Client: "krowk-mcp/" + s.Version})

	lines := []string{"Run context detected for this working directory:"}
	for _, f := range [][2]string{
		{"repo", metadata.Repo},
		{"commit", metadata.Commit},
		{"branch", metadata.Branch},
		{"agent", metadata.Agent},
		{"pull request", metadata.PullRequest},
	} {
		if f[1] != "" {
			lines = append(lines, fmt.Sprintf("  %-12s %s", f[0], f[1]))
		}
	}
	if len(lines) == 1 {
		lines = append(lines, "  nothing detected — not a git checkout, or no remote")
	}
	lines = append(lines,
		"", "Every push attaches this automatically; pass repo, commit or agent to override.")
	return strings.Join(lines, "\n"), metadata, nil
}

func verifyKey(ctx context.Context, s *Server, _ json.RawMessage) (string, any, error) {
	if s.Client.Token == "" {
		return "No API key is configured, so pushes will be anonymous: they expire in 24h " +
				"and come back with a claim URL.\n\nSet KROWK_TOKEN, or run `krowk auth login --token krk_...`.",
			map[string]any{"authenticated": false}, nil
	}

	key, err := s.Client.VerifyKey(ctx)
	if err != nil {
		return "", nil, err
	}
	lines := []string{
		"Key " + key.KeyID + " is valid.",
		"  workspace  " + key.Workspace,
		"  scopes     " + strings.Join(key.Scopes, " "),
	}
	if !key.HasScope(api.ScopeWrite) {
		lines = append(lines, "", "This key cannot upload — it is missing "+api.ScopeWrite+".")
	}
	return strings.Join(lines, "\n"), key, nil
}

// render is the one place a result turns into text, so every tool hands back
// both paste forms described the same way.
func (s *Server) render(a *api.Artifact, title string) (string, any, error) {
	paste := output.PasteFor(a, title)

	lines := []string{
		fmt.Sprintf("Artifact %s — %s", a.ID, output.HumanBytes(a.Bytes)),
	}
	if expiry := output.RelativeExpiry(a.ExpiresAt, s.Now()); expiry != "" {
		lines = append(lines, expiry)
	}
	lines = append(lines,
		"",
		"Paste into "+output.EmbedSurfaces+":",
		paste.Markdown,
		"",
		"Paste into "+output.LinkSurfaces+":",
		paste.URL,
	)
	if a.ClaimURL != "" {
		lines = append(lines,
			"",
			"This upload is anonymous and nobody owns it yet. The link below adopts it —",
			"it is a secret, so hand it to the human and do not paste it anywhere public:",
			a.ClaimURL,
		)
	}

	return strings.Join(lines, "\n"), map[string]any{
		"artifact": a,
		"paste":    paste,
	}, nil
}

// ---- schemas ----

func toolSchemas() []map[string]any {
	return []map[string]any{
		{
			"name": "krowk_push",
			"description": "Upload one or more local files and get back a permalink that unfurls " +
				"with the run metadata attached. Returns both paste-ready forms: the markdown " +
				"image embed for GitHub, Linear and Notion, and the bare URL for Slack and " +
				"Basecamp. Repo, commit, branch and agent are detected automatically.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"files"},
				"properties": map[string]any{
					"files": map[string]any{
						"type":     "array",
						"items":    map[string]any{"type": "string"},
						"minItems": 1,
						"description": "Paths to upload, resolved from the working directory. Must be " +
							"inside it — paths outside are refused, symlinks included, and credential " +
							"files are refused even inside it, because an artifact is readable by " +
							"anyone with the link.",
					},
					"title": map[string]any{
						"type":        "string",
						"description": "Label for the unfurl card and the markdown link text.",
					},
					"pull_request": map[string]any{
						"type":        "string",
						"description": "URL of the pull request this work belongs to.",
					},
					"reference": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Related links, e.g. the issue being fixed.",
					},
					"session": map[string]any{
						"type":        "string",
						"description": "Agent session ID, to group a run's artifacts.",
					},
					"repo":   map[string]any{"type": "string", "description": "Override the detected repository."},
					"commit": map[string]any{"type": "string", "description": "Override the detected commit."},
					"agent":  map[string]any{"type": "string", "description": "Override the detected agent name."},
				},
				"additionalProperties": false,
			},
		},
		{
			"name": "krowk_get_artifact",
			"description": "Look up an artifact already uploaded, by the ID at the end of its link. " +
				"Returns the same paste-ready forms as a push.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"id"},
				"properties": map[string]any{
					"id": map[string]any{
						"type":        "string",
						"description": "Artifact ID, the last part of the URL — e.g. 9f3c2e1.",
					},
				},
				"additionalProperties": false,
			},
		},
		{
			"name": "krowk_get_run",
			"description": "Report the repository, commit, branch and agent that will be attached " +
				"to the next push. Useful for checking detection before uploading.",
			"inputSchema": map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			},
		},
		{
			"name": "krowk_verify_key",
			"description": "Check whether an API key is configured and what it is allowed to do. " +
				"Without a key, pushes still work but are anonymous and expire in 24h.",
			"inputSchema": map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			},
		},
	}
}

// ---- JSON-RPC ----

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// isNotification reports whether the peer wants no reply.
func (r request) isNotification() bool {
	s := strings.TrimSpace(string(r.ID))
	return s == "" || s == "null"
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}
