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
	"bytes"
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
const instructions = `krowk turns local files into permalinks, one artifact per file, grouped under a
run that carries the metadata about where they came from.

Call krowk_push with the paths you want to share. Every artifact comes back in
two paste-ready forms and you must pick by destination:

  - markdown  ` + output.EmbedSurfaces + `
  - url       ` + output.LinkSurfaces + `

Pasting the markdown form into Slack shows raw text; pasting the bare URL into a
GitHub comment shows a plain link with no image. Neither surface renders the
other's form, so choose deliberately.

If a push comes back anonymous, each artifact carries a claim_token. Spending it
— krowk_claim_artifact, or ` + "`krowk claim <slug> <token>`" + ` — is the only way to
keep that upload past its expiry, and anyone holding the token can do it. Treat
it as a secret: give it to the human, never paste it into a pull request, an
issue or a chat message. An anonymous upload belongs to no run, so pass the run
to krowk_claim_artifact if the upload should be grouped with the rest of them.

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
	// WorkspaceErr is why this server holds no key when it was supposed to hold
	// one: a configuration file it could not read, or a workspace the config
	// pinned that has no key stored for it. It is nil in the ordinary anonymous
	// case, where having no key is the intended state rather than a failure.
	//
	// It exists because the alternative to serving is not serving: an artifact
	// needs no credential to read, and an anonymous push plus a claim token is a
	// whole working flow, so refusing to start takes those away over a problem
	// they do not have. What it must not do is let an upload land in the
	// anonymous workspace when a repository asked for a named one — silently
	// putting the file somewhere other than where the config said. So the tools
	// that would create something there refuse with this instead.
	WorkspaceErr error
	// Now is swapped out in tests so expiry text is stable.
	Now func() time.Time
}

// resolveRoot is the confinement boundary, fully resolved on every call — so a
// process that changes directory moves the boundary with it.
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
	// A root sitting inside a secret-named directory is no boundary either:
	// secretPath only inspects components below the root, so a server started in
	// ~/.ssh would otherwise make config and authorized_keys pushable.
	if name := secretComponent(abs); name != "" {
		return "", api.Fail("root_too_broad",
			"the upload root is inside "+name+", which holds credentials — "+
				"start the server in a project directory, or pass --root")
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

// secretPath reports whether any part of the path below root is one of those
// names, so a directory match covers everything under it.
func secretPath(root, resolved string) string {
	rel, err := filepath.Rel(root, resolved)
	if err != nil {
		return ""
	}
	return secretComponent(rel)
}

// secretComponent returns the first path component that names a secret, if any.
func secretComponent(path string) string {
	for part := range strings.SplitSeq(path, string(filepath.Separator)) {
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
	// A hard link is the one escape resolving symlinks cannot see: a second name
	// for the same inode, so a path inside the root can be a key outside it with
	// nothing to resolve. Nothing says where the other names are, so a file with
	// any is refused. Screenshots do not have them; package stores do, which is
	// why this rejects node_modules rather than anything worth publishing.
	if info, err := os.Stat(real); err == nil && info.Mode().IsRegular() && multiplyLinked(info) {
		return "", api.Fail("hard_linked",
			"`"+path+"` has more than one name on disk, so it may be a file from outside "+root+
				" — copy it in and push the copy")
	}
	return real, nil
}

// Serve reads requests until the stream ends or the context is cancelled.
//
// The read runs in its own goroutine feeding a channel, because a blocked read
// on stdin would otherwise keep a cancelled server alive forever: returning
// from here exits the process, and the reader dies with it.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	if s.Now == nil {
		s.Now = time.Now
	}

	writer := bufio.NewWriter(out)
	encoder := json.NewEncoder(writer)
	lines := readLines(ctx, in)

	for {
		var line []byte
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-lines:
			if !ok {
				return nil
			}
			if msg.err != nil {
				return msg.err
			}
			line = msg.line
		}
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}

		var req request
		if json.Unmarshal(line, &req) != nil {
			// Nothing to correlate a reply to, so answer with a null id.
			// -32700 is only for text that fails to parse as JSON; a line that
			// parses but is not a Request object — a batch array, a bare
			// string — is an invalid request, -32600.
			rpcErr := &rpcError{Code: -32700, Message: "the message is not JSON"}
			if json.Valid(line) {
				rpcErr = &rpcError{Code: -32600, Message: "the message is JSON but not a Request object"}
			}
			if err := write(encoder, writer, response{
				JSONRPC: "2.0",
				ID:      json.RawMessage("null"),
				Error:   rpcErr,
			}); err != nil {
				return err
			}
			continue
		}
		if req.Method == "" {
			// null, {} and an id with no method all unmarshal cleanly, but a
			// Request must carry a method and "" is never a legitimate one.
			if err := write(encoder, writer, response{
				JSONRPC: "2.0",
				ID:      json.RawMessage("null"),
				Error:   &rpcError{Code: -32600, Message: "the message is JSON but not a Request object"},
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

// message is one line off the stream, or the error that ended it.
type message struct {
	line []byte
	err  error
}

// readLines scans the stream line by line on its own goroutine. The scanner's
// buffer enforces maxLine while reading, so an oversized message is rejected
// at the cap instead of being allocated in full first.
func readLines(ctx context.Context, in io.Reader) <-chan message {
	lines := make(chan message)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(in)
		scanner.Buffer(make([]byte, 0, 64<<10), maxLine)
		for scanner.Scan() {
			line := make([]byte, len(scanner.Bytes()))
			copy(line, scanner.Bytes())
			select {
			case lines <- message{line: line}:
			case <-ctx.Done():
				return
			}
		}
		err := scanner.Err()
		if errors.Is(err, bufio.ErrTooLong) {
			err = fmt.Errorf("mcp: message longer than %d bytes", maxLine)
		}
		if err != nil {
			select {
			case lines <- message{err: err}:
			case <-ctx.Done():
			}
		}
	}()
	return lines
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
		return s.initialize(req.Params)
	case "notifications/initialized":
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

func (s *Server) initialize(params json.RawMessage) (any, *rpcError) {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	// A client that cannot frame initialize is the one case worth surfacing.
	if len(params) > 0 && json.Unmarshal(params, &p) != nil {
		return nil, &rpcError{Code: -32602, Message: "initialize params are not an object"}
	}

	return map[string]any{
		"protocolVersion": negotiate(p.ProtocolVersion),
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "krowk", "version": s.Version},
		"instructions":    instructions,
	}, nil
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

// workspaceReason renders WorkspaceErr as one line. An *api.Error's Error() is
// only its code, and the code alone ("no_stored_key") does not tell anyone what
// to do, so the fix comes with it.
func workspaceReason(err error) string {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		return err.Error()
	}
	if fix := apiErr.Fix(); fix != "" {
		return apiErr.Code() + " — " + fix
	}
	return apiErr.Code()
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
	"krowk_push":           push,
	"krowk_list_artifacts": listArtifacts,
	"krowk_get_artifact":   getArtifact,
	"krowk_claim_artifact": claimArtifact,
	"krowk_get_run":        getRun,
	"krowk_verify_key":     verifyKey,
}

// given names the arguments that were actually passed, in the schema's own
// order, so a note about dropped metadata lists what the caller sent and
// nothing else.
func given(passed map[string]bool) []string {
	order := []string{"pull_request", "links", "references", "session", "title", "metadata"}
	var names []string
	for _, name := range order {
		if passed[name] {
			names = append(names, "`"+name+"`")
		}
	}
	return names
}

// checkLinkFields re-reads just the links, refusing a field the schema does not
// name. Separate from the main decode so the strictness reaches the links and
// nothing else.
func checkLinkFields(args json.RawMessage) error {
	var probe struct {
		Links json.RawMessage `json:"links"`
	}
	if json.Unmarshal(args, &probe) != nil || len(probe.Links) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(probe.Links))
	dec.DisallowUnknownFields()
	var links []runctx.Link
	if err := dec.Decode(&links); err != nil {
		return err
	}
	return nil
}

// badArgument names the argument that did not fit its type, so an agent fixes
// the one it got wrong. Without the name every shape error read as "`files`
// must be an array of paths" — advice to change the argument that was right.
func badArgument(err error) string {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) && typeErr.Field != "" {
		return fmt.Sprintf("`%s` is the wrong shape: got %s — see the tool's schema",
			typeErr.Field, typeErr.Value)
	}
	// The decoder's own wording for an unknown field already names it, and no
	// paraphrase of it says more: `json: unknown field "label"`.
	if msg := err.Error(); strings.Contains(msg, "unknown field") {
		return strings.TrimPrefix(msg, "json: ") +
			" — this tool takes only the arguments in its schema"
	}
	return "the arguments are not the shape this tool takes: `files` is an array of " +
		"paths, `links` an array of {url, title, rel} objects — see the tool's schema"
}

func push(ctx context.Context, s *Server, args json.RawMessage) (string, any, error) {
	var a struct {
		Files       []string          `json:"files"`
		Run         string            `json:"run"`
		Title       string            `json:"title"`
		PullRequest string            `json:"pull_request"`
		Links       []runctx.Link     `json:"links"`
		References  []string          `json:"references"`
		Session     string            `json:"session"`
		Repo        string            `json:"repo"`
		Commit      string            `json:"commit"`
		Agent       string            `json:"agent"`
		Metadata    map[string]string `json:"metadata"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return "", nil, api.Fail("bad_arguments", badArgument(err))
		}
		// A link's own fields are checked strictly, because a link is the one
		// argument here whose misspelling is silent: `label` where the schema says
		// `title` decodes to a link with no name and nothing said about it. The
		// arguments around it stay lenient, the way every other tool's are — a
		// stray one is not worth failing an upload over.
		if err := checkLinkFields(args); err != nil {
			return "", nil, api.Fail("bad_arguments", badArgument(err))
		}
	}
	if len(a.Files) == 0 {
		return "", nil, api.Fail("no_file", "pass at least one path in `files`")
	}
	// Held to the vocabulary here rather than at the registry, which stores
	// metadata verbatim and validates nothing but its size: a malformed link
	// accepted now is malformed in the record forever.
	if err := runctx.ValidateLinks(a.Links); err != nil {
		return "", nil, api.Fail("bad_arguments", "`links`: "+err.Error())
	}
	// A push creates the thing the workspace was pinned for. Uploading it
	// anonymously instead would be the misdirection this refusal exists to
	// prevent: the file would be published, the link would work, and it would be
	// in the wrong place with nothing saying so. Checked before the arguments are
	// shaped, so an agent told to fix its `run` does not learn about a broken
	// workspace on the next call instead.
	if s.WorkspaceErr != nil {
		return "", nil, s.WorkspaceErr
	}
	// A run named by link is a run named: the agent holding one usually holds the
	// result of the call that opened it, links and all.
	runNamed, err := api.ParseSlug(api.KindRun, a.Run)
	if err != nil {
		return "", nil, err
	}
	a.Run = runNamed

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

	// Every file is measured and digested before anything is sent, so a bad
	// path in the last place fails before the first upload rather than halfway.
	specs := make([]api.Spec, 0, len(files))
	for _, path := range files {
		spec, err := api.Inspect(path)
		if err != nil {
			return "", nil, err
		}
		specs = append(specs, spec)
	}

	var notes []string
	runSlug, ownRun := a.Run, false
	var run *api.Run
	resolved := runctx.Resolve(s.Env, runctx.Overrides{
		Repo:        a.Repo,
		Commit:      a.Commit,
		Agent:       a.Agent,
		PullRequest: a.PullRequest,
		Links:       a.Links,
		References:  a.References,
		Session:     a.Session,
		Title:       a.Title,
		Client:      "krowk-mcp/" + s.Version,
	})
	if runSlug == "" && s.Client.Authenticated() {
		created, err := s.Client.CreateRun(ctx, resolved)
		if err != nil {
			return "", nil, err
		}
		run, runSlug, ownRun = created, created.Slug, true
	}
	// Named rather than listed wholesale: an agent told that four arguments it
	// never passed were dropped has to work out which of them it actually sent.
	work := given(map[string]bool{
		"pull_request": a.PullRequest != "",
		"links":        len(a.Links) > 0,
		"references":   len(a.References) > 0,
		"session":      a.Session != "",
		"title":        a.Title != "",
	})
	if !s.Client.Authenticated() {
		if all := given(map[string]bool{"metadata": len(a.Metadata) > 0}); len(work)+len(all) > 0 {
			notes = append(notes, strings.Join(append(work, all...), ", ")+" were not recorded: "+
				"a keyless upload records no metadata, and opening a run needs an API key")
		}
	} else if a.Run != "" && len(work) > 0 {
		// A run named by the caller already carries the metadata it was opened
		// with, so these have nowhere to land. Said rather than refused: the same
		// arguments on every push of a batch is a reasonable way to write the
		// calls, and `metadata` still lands on each artifact.
		notes = append(notes, strings.Join(work, ", ")+" were not recorded: run "+a.Run+
			" already carries the metadata it was opened with")
	}

	// Each artifact carries its own production record, stamped at this moment;
	// the run keeps the facts about the work. Keyless pushes record neither.
	var stamp any
	if s.Client.Authenticated() {
		stamp = resolved.Artifact().WithExtras(a.Metadata)
	}

	artifacts := make([]*api.Artifact, 0, len(specs))
	for _, spec := range specs {
		spec.Run = runSlug
		spec.Metadata = stamp
		artifact, err := s.Client.Push(ctx, spec)
		if err != nil {
			return "", nil, withProgress(err, artifacts, runSlug, ownRun)
		}
		artifacts = append(artifacts, artifact)
	}

	// A run this call opened is a run this call closes. Failing to close it is
	// not worth failing the push over — the links work — but it is worth saying.
	if ownRun {
		if finished, err := s.Client.FinishRun(ctx, runSlug); err == nil {
			run = finished
		} else {
			notes = append(notes, "run "+runSlug+" could not be finished — retry `krowk runs finish "+runSlug+"`")
		}
	}

	return s.renderPush(artifacts, run, notes)
}

// withProgress keeps what a failed batch would otherwise lose: the links of
// whatever did upload, and the run this call opened.
func withProgress(err error, done []*api.Artifact, runSlug string, ownRun bool) error {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		return err
	}
	if len(done) > 0 {
		urls := make([]string, 0, len(done))
		for _, a := range done {
			urls = append(urls, a.URL)
		}
		apiErr.Body["uploaded_before_failure"] = urls
	}
	if ownRun {
		apiErr.Body["run"] = runSlug
		finish := "the run is still open — close it with `krowk runs finish " + runSlug + "`"
		if fix, _ := apiErr.Body["fix"].(string); fix != "" {
			finish = fix + "; " + finish
		}
		apiErr.Body["fix"] = finish
	}
	return apiErr
}

// withClaimed says what a failed attach leaves behind: the claim succeeded and
// the token is spent, so the upload is kept and only the run is missing.
//
// The retry it names is this tool again, not the CLI's `uploads attach` — an agent
// on this transport is here precisely because it cannot shell out, and advice it
// cannot follow is worse than none. Calling claim again works because a repeat
// claim with the same key is the same success rather than a spent-token error, so
// the whole call can simply be made again with a run that exists.
func withClaimed(err error, claimed *api.Artifact) error {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		return err
	}
	apiErr.Body["claimed"] = claimed.Slug
	retry := "the upload is claimed and kept, only the run is not attached — call " +
		"krowk_claim_artifact again with the same slug and claim_token, and a run this " +
		"workspace holds; claiming twice with the same key is the same success"
	if fix, _ := apiErr.Body["fix"].(string); fix != "" {
		retry = fix + "; " + retry
	}
	apiErr.Body["fix"] = retry
	return apiErr
}

func listArtifacts(ctx context.Context, s *Server, args json.RawMessage) (string, any, error) {
	var a struct {
		Limit  int    `json:"limit"`
		Before string `json:"before"`
	}
	if len(args) > 0 && json.Unmarshal(args, &a) != nil {
		return "", nil, api.Fail("bad_arguments", "`limit` must be a number and `before` a slug")
	}
	// This one creates nothing, but the only listing it could return is the
	// pinned workspace's, and without a key the registry would answer
	// `unauthorized` — true, and no help at all in finding the malformed config
	// or the missing key that caused it. Say the real reason instead.
	if s.WorkspaceErr != nil {
		return "", nil, s.WorkspaceErr
	}

	page, err := s.Client.ListArtifacts(ctx, strings.TrimSpace(a.Before), a.Limit)
	if err != nil {
		return "", nil, err
	}

	lines := make([]string, 0, len(page.Artifacts)+2)
	if len(page.Artifacts) == 0 {
		lines = append(lines, "No artifacts in this workspace yet.")
	}
	for _, artifact := range page.Artifacts {
		line := fmt.Sprintf("%s  %s  %s  %s",
			artifact.Slug, artifact.Filename, output.HumanBytes(artifact.ByteSize), artifact.URL)
		if artifact.State != "ready" {
			line += "  (" + artifact.State + ")"
		}
		lines = append(lines, line)
	}
	if page.Next != "" {
		lines = append(lines, "", "More: pass before="+page.Next+" for the next page.")
	}
	return strings.Join(lines, "\n"), page, nil
}

// getArtifact reads, and reading needs no credential — an artifact is published
// at a URL anyone with the link can fetch. So it works unchanged when
// WorkspaceErr is set, as does krowk_get_run, which never touches the registry
// at all. A server that cannot resolve its workspace is still useful for these.
func getArtifact(ctx context.Context, s *Server, args json.RawMessage) (string, any, error) {
	var a struct {
		Slug string `json:"slug"`
	}
	if len(args) > 0 && json.Unmarshal(args, &a) != nil {
		return "", nil, api.Fail("bad_arguments", "`slug` must be a string")
	}
	if strings.TrimSpace(a.Slug) == "" {
		return "", nil, api.Fail("missing_slug", "pass the artifact slug, e.g. art_...")
	}
	slug, err := api.ParseSlug(api.KindArtifact, a.Slug)
	if err != nil {
		return "", nil, err
	}

	artifact, err := s.Client.ShowArtifact(ctx, slug)
	if err != nil {
		return "", nil, err
	}
	return s.render(artifact)
}

// claimArtifact adopts an anonymous upload, and optionally puts it under a run on
// the way — the agent spending the claim token is the one that knows which run the
// upload came from, so there is no separate attach tool. The order is fixed: the
// attach resolves both slugs in the key's workspace, so the claim has to land
// first.
func claimArtifact(ctx context.Context, s *Server, args json.RawMessage) (string, any, error) {
	var a struct {
		Slug       string `json:"slug"`
		ClaimToken string `json:"claim_token"`
		Run        string `json:"run"`
	}
	if len(args) > 0 && json.Unmarshal(args, &a) != nil {
		return "", nil, api.Fail("bad_arguments", "`slug`, `claim_token` and `run` must be strings")
	}
	// A blank slug is answered as the argument it is, not as missing_claim, which
	// is classified as a credential to produce — the same split the CLI makes,
	// because an agent on either transport is told to check its key otherwise.
	if strings.TrimSpace(a.Slug) == "" {
		return "", nil, api.Fail("no_artifact", "pass the artifact slug, e.g. art_...")
	}
	if strings.TrimSpace(a.ClaimToken) == "" {
		return "", nil, api.Fail("missing_claim", "pass the claim_token the anonymous push returned")
	}
	// A claim moves an upload into the key's workspace and keeps it there, so it
	// is a creation in the pinned workspace by another name — and the token is
	// one-shot, so a claim that lands in the wrong workspace cannot be redone.
	// Refuse before spending it, and before anything else: an agent told to fix
	// its `run` argument first would learn about the broken workspace second.
	if s.WorkspaceErr != nil {
		return "", nil, s.WorkspaceErr
	}
	slug, err := api.ParseSlug(api.KindArtifact, a.Slug)
	if err != nil {
		return "", nil, err
	}
	runSlug, err := api.ParseSlug(api.KindRun, a.Run)
	if err != nil {
		return "", nil, err
	}

	artifact, err := s.Client.ClaimArtifact(ctx, slug, strings.TrimSpace(a.ClaimToken))
	if err != nil {
		return "", nil, err
	}

	if runSlug != "" {
		// The claim has already been spent, so a failure here must not read as one
		// the agent can undo by calling the tool again: the artifact is kept, and
		// only the run is missing.
		attached, err := s.Client.AttachRun(ctx, artifact.Slug, runSlug)
		if err != nil {
			return "", nil, withClaimed(err, artifact)
		}
		artifact = attached
	}

	text, structured, err := s.render(artifact)
	if err != nil {
		return "", nil, err
	}
	if runSlug != "" {
		// renderPush's line, without the status it puts in brackets: attaching answers
		// with the artifact, so the run's state is not among the facts in hand, and a
		// second call to fetch it would buy nothing the agent asked for.
		text += "\n\nGrouped under run " + runSlug + "."
	} else if artifact.RunSlug() == "" {
		// The same thing the CLI's claim hands back: the upload is kept but belongs
		// to no run, and nothing else will ever put it in one. There are no
		// breadcrumbs on this transport, so it is a line rather than a crumb — but
		// it is said, instead of being left in the tool schema for an agent that has
		// already stopped reading it.
		//
		// The way out named here is this tool again, the same as withClaimed's: an
		// agent on this transport cannot shell out to `uploads attach`, and a repeat
		// claim with the same key and token is the same success rather than a
		// spent-token error, so the whole call can simply be made again with a run.
		text += "\n\nIt belongs to no run. A run is where the pull request, commit and " +
			"session are recorded — call krowk_claim_artifact again with the same slug and " +
			"claim_token and a `run` this workspace holds to group it under one; claiming " +
			"twice with the same key is the same success."
	}
	return text, structured, nil
}

func getRun(_ context.Context, s *Server, _ json.RawMessage) (string, any, error) {
	metadata := runctx.Resolve(s.Env, runctx.Overrides{Client: "krowk-mcp/" + s.Version})

	lines := []string{"Run context detected for this working directory:"}
	for _, f := range [][2]string{
		{"repo", metadata.RepoName},
		{"commit", metadata.Commit},
		{"branch", metadata.Branch},
		{"agent", metadata.Harness},
		{"pull request", metadata.ChangeURL},
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
	// The question this tool answers is where uploads land, and "nowhere, for
	// this reason" is an answer rather than a failure — an agent that just had a
	// push refused reads it here to find out why, so it is reported, not raised.
	if s.WorkspaceErr != nil {
		return "This server has no API key, and it was meant to have one: " + workspaceReason(s.WorkspaceErr) +
				"\n\nUploads, claims and listings are refused rather than landing in the anonymous " +
				"workspace, because this checkout's config names a workspace of its own. Looking up " +
				"an artifact still works. Fix the key or the config and restart the server.",
			map[string]any{"authenticated": false, "workspace_error": workspaceReason(s.WorkspaceErr)}, nil
	}
	if s.Client.Token == "" {
		return "No API key is configured, so pushes will be anonymous: they expire within a day " +
				"and come back with a claim token.\n\nSet KROWK_TOKEN, or run `krowk auth login --token krowk_sk_...`.",
			map[string]any{"authenticated": false}, nil
	}

	key, err := s.Client.VerifyKey(ctx)
	if err != nil {
		return "", nil, err
	}
	lines := []string{
		"Key " + key.KeyID + " is valid.",
		"  workspace  " + key.Workspace,
	}
	if key.Name != "" {
		lines = append(lines, "  name       "+key.Name)
	}
	if key.ExpiresAt != "" {
		lines = append(lines, "  expires    "+key.ExpiresAt)
	}
	return strings.Join(lines, "\n"), key, nil
}

// render is the one place a lone artifact turns into text, so every tool hands
// back both paste forms described the same way.
func (s *Server) render(a *api.Artifact) (string, any, error) {
	paste := output.PasteOf(a)

	lines := []string{
		fmt.Sprintf("Artifact %s — %s, %s", a.Slug, a.Filename, output.HumanBytes(a.ByteSize)),
	}
	if expiry := output.RelativeExpiry(a.ExpiresAt, s.Now()); expiry != "" {
		lines = append(lines, expiry)
	}
	lines = append(lines, pasteLines(paste)...)
	lines = append(lines, claimLines(a)...)

	return strings.Join(lines, "\n"), map[string]any{
		"artifact": a,
		"paste":    paste,
	}, nil
}

// renderPush reports a whole push: one artifact per file, each with both paste
// forms, and the run they were grouped under when there was one.
func (s *Server) renderPush(artifacts []*api.Artifact, run *api.Run, notes []string) (string, any, error) {
	var lines []string
	pastes := make([]output.Paste, 0, len(artifacts))
	for i, a := range artifacts {
		if i > 0 {
			lines = append(lines, "")
		}
		paste := output.PasteOf(a)
		pastes = append(pastes, paste)
		lines = append(lines,
			fmt.Sprintf("Artifact %s — %s, %s", a.Slug, a.Filename, output.HumanBytes(a.ByteSize)))
		if expiry := output.RelativeExpiry(a.ExpiresAt, s.Now()); expiry != "" {
			lines = append(lines, expiry)
		}
		lines = append(lines, pasteLines(paste)...)
		lines = append(lines, claimLines(a)...)
	}
	if run != nil {
		lines = append(lines, "", "Grouped under run "+run.Slug+" ("+run.Status+").")
	}
	for _, note := range notes {
		lines = append(lines, "", "Note: "+note)
	}

	structured := map[string]any{
		"artifacts": artifacts,
		"pastes":    pastes,
	}
	if run != nil {
		structured["run"] = run
	}
	if len(notes) > 0 {
		structured["notes"] = notes
	}
	return strings.Join(lines, "\n"), structured, nil
}

// pasteLines shows both paste forms, labelled honestly: the markdown label only
// promises an image where the markdown actually embeds one.
func pasteLines(paste output.Paste) []string {
	return []string{
		"",
		"Paste into " + output.MarkdownSurfacesFor(paste) + ":",
		paste.Markdown,
		"",
		"Paste into " + output.LinkSurfaces + ":",
		paste.URL,
	}
}

// claimLines carries the one-shot claim token of an anonymous upload, with the
// warning it deserves.
//
// The command comes from output.ClaimCrumb, which is exported for exactly this:
// the CLI's envelope, the CLI's human line and this server all name the same
// call, and spelling it out a fourth time here is how the four drift apart.
func claimLines(a *api.Artifact) []string {
	if a.ClaimToken == "" {
		return nil
	}
	return []string{
		"",
		"This upload is anonymous and nobody owns it yet. The command below adopts it —",
		"the token is a secret, so hand it to the human and do not paste it anywhere public:",
		output.ClaimCrumb(a).Cmd,
	}
}

// ---- schemas ----

func toolSchemas() []map[string]any {
	return []map[string]any{
		{
			"name": "krowk_push",
			"description": "Upload one or more local files; every file becomes its own artifact with " +
				"a permalink, grouped under a run when an API key is configured. Returns both " +
				"paste-ready forms per artifact: the markdown image embed for GitHub, Linear and " +
				"Notion, and the bare URL for Slack and Basecamp. Repo, commit, branch and agent " +
				"are detected automatically.",
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
					"run": map[string]any{
						"type": "string",
						"description": "Attach to an existing run instead of opening one. Its slug, " +
							"or any link carrying it.",
					},
					"title": map[string]any{
						"type":        "string",
						"description": "Title for the run this push opens, e.g. the pull request's.",
					},
					"pull_request": map[string]any{
						"type":        "string",
						"description": "URL of the pull request this work belongs to.",
					},
					"links": map[string]any{
						"type":     "array",
						"maxItems": runctx.MaxLinks,
						"items": map[string]any{
							"type":     "object",
							"required": []string{"url"},
							"properties": map[string]any{
								"url": map[string]any{
									"type":        "string",
									"format":      "uri",
									"maxLength":   runctx.MaxLinkURL,
									"description": "Absolute http(s) URL. Anything else is refused, not trimmed.",
								},
								"title": map[string]any{
									"type":      "string",
									"maxLength": runctx.MaxLinkTitle,
									"description": "One line naming the link, shown to a reader instead of " +
										"the URL, e.g. the issue's own title.",
								},
								"rel": map[string]any{
									"type":      "string",
									"maxLength": runctx.MaxLinkRel,
									"description": "What this link is. Pick from " +
										strings.Join(runctx.LinkRels, ", ") +
										" where one fits: `fixes` for the issue this work closes, `tracks` " +
										"for the ticket it is filed under, `spec` for what it implements, " +
										"`discussion` for the thread about it, `source` for what it was " +
										"derived from, `supersedes` for the run it replaces. Any other word " +
										"is accepted and stored as given.",
								},
							},
							"additionalProperties": false,
						},
						"description": "Links this work is about, recorded on the run as `krowk.links` — " +
							"the issue being fixed, the spec, the discussion. Prefer this over " +
							"`references` for anything that is a URL.",
					},
					"references": map[string]any{
						"type":  "array",
						"items": map[string]any{"type": "string"},
						"description": "Related identifiers that are not URLs, e.g. a ticket key. " +
							"A URL belongs in `links`.",
					},
					"session": map[string]any{
						"type":        "string",
						"description": "Override the detected agent session. Recorded on the run.",
					},
					"repo":   map[string]any{"type": "string", "description": "Override the detected repository."},
					"commit": map[string]any{"type": "string", "description": "Override the detected commit."},
					"agent":  map[string]any{"type": "string", "description": "Override the detected agent name."},
					"metadata": map[string]any{
						"type":                 "object",
						"additionalProperties": map[string]any{"type": "string"},
						"description": "Extra key/value metadata recorded on each artifact, e.g. " +
							"krowk.caption or url.full. Your value wins over a detected one. " +
							"Metadata is public: anyone with the link can read it.",
					},
				},
				"additionalProperties": false,
			},
		},
		{
			"name": "krowk_list_artifacts",
			"description": "List the workspace's artifacts, newest first. Needs an API key: keyless " +
				"uploads share the anonymous workspace, so there is nothing of one's own to list.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"limit": map[string]any{
						"type":        "integer",
						"description": "Artifacts per page (1–100, default 50).",
					},
					"before": map[string]any{
						"type":        "string",
						"description": "Start after this artifact slug — the `next` of the last page.",
					},
				},
				"additionalProperties": false,
			},
		},
		{
			"name": "krowk_get_artifact",
			"description": "Look up an artifact already uploaded, by its slug. " +
				"Returns the same paste-ready forms as a push.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"slug"},
				"properties": map[string]any{
					"slug": map[string]any{
						"type": "string",
						"description": "Artifact slug, e.g. art_9f3c2e1a7b04d6c8e5f1a2b3 — or any " +
							"link carrying it, like https://krowk.com/a/art_9f3c2e1a7b04d6c8e5f1a2b3.",
					},
				},
				"additionalProperties": false,
			},
		},
		{
			"name": "krowk_claim_artifact",
			"description": "Spend a claim token to move an anonymous artifact into the key's " +
				"workspace, where it stops expiring. Pass `run` to also group it under a run — " +
				"an anonymous upload could not name one, so this is the only way it gets one. " +
				"Needs an API key.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"slug", "claim_token"},
				"properties": map[string]any{
					"slug": map[string]any{
						"type": "string",
						"description": "Artifact slug, e.g. art_9f3c2e1a7b04d6c8e5f1a2b3 — or any " +
							"link carrying it, like https://krowk.com/a/art_9f3c2e1a7b04d6c8e5f1a2b3.",
					},
					"claim_token": map[string]any{
						"type":        "string",
						"description": "The claim token the anonymous push returned, e.g. krowk_claim_...",
					},
					"run": map[string]any{
						"type": "string",
						"description": "Run to attach the claimed upload to, e.g. run_..., or any " +
							"link carrying it. Attached after the claim, so it must be a run in this " +
							"key's workspace.",
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
			"description": "Check whether an API key is configured, and which workspace uploads " +
				"made with it land in. Without a key, pushes still work but are anonymous and expire in 24h. " +
				"Call it when a push is refused: if this checkout pinned a workspace the server could not " +
				"reach, the reason is here.",
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
