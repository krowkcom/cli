package claude

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/krowkcom/cli/internal/harness"
	"github.com/krowkcom/cli/internal/importer"
)

// The names this package answers to. Provider is what ran the model and
// Harness is what drove it; see the package doc for why Binding.Provider is
// not Provider but importer.ProviderClaude.
const (
	// Provider is the model vendor, written into session.provider and
	// every message.provider.
	Provider = "anthropic"
	// Harness is the tool that produced the transcript.
	Harness = "claude"
)

// projectsDir is where Claude Code keeps transcripts, relative to home. It
// is joined under the home directory through importer.HomePath, so a
// symlink pointing out of home is refused rather than followed.
const projectsDir = ".claude/projects"

// subagentsDir is the directory beside a session's transcript holding the
// transcripts of the agents it dispatched.
const subagentsDir = "subagents"

// agentFilePrefix is what a subagent transcript's filename starts with. The
// rest of the stem is the agent id, which is also the `agentId` field on
// every line of the file — Discover uses the cheap half of that equivalence
// so it does not have to open every file to list one.
const agentFilePrefix = "agent-"

// Source is the Claude importer. It holds nothing: every read takes the
// harness.Env it should resolve home through, so a test points it at a
// temporary directory rather than at the developer's own transcripts.
type Source struct{}

// Source really is one, checked at compile time rather than at the call
// site that first tries to use it as one.
var _ importer.Source = Source{}

// Name is the provider key, which is the store's name for Claude rather
// than Anthropic's — see the package doc.
func (Source) Name() string { return importer.ProviderClaude }

// Discover lists every session transcript under the home directory, with
// each session's subagent transcripts immediately after it.
//
// The ordering is not cosmetic. A subagent's Thread names its parent by
// binding, and the store can only turn that into a parent_id once the
// parent's binding exists, so a caller that ingests these refs in the order
// they come back never leaves a NULL behind. Sorting is by name at both
// levels so two runs on an unchanged machine produce the same list, which is
// what makes a golden test of a discovery possible at all.
//
// Being cheap and slightly wrong is allowed here: a directory that cannot be
// read is skipped rather than failing the whole discovery, because a
// transcript directory being replaced under the walk is ordinary and losing
// every other session over it is not. Only the projects directory itself
// failing to resolve is an error, and its absence is not even that — a
// machine with no Claude on it has no transcripts, which is an answer.
func (s Source) Discover(env harness.Env) ([]importer.Ref, error) {
	if err := importer.CheckOS(); err != nil {
		return nil, fmt.Errorf("claude: %w", err)
	}
	root, err := importer.HomePath(env, projectsDir)
	if err != nil {
		// errors.Is rather than os.IsNotExist: HomePath wraps with %w,
		// and the older predicate does not unwrap, so a machine with no
		// Claude on it would have surfaced as an error rather than as an
		// empty list.
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("claude: resolve projects directory: %w", err)
	}
	slugs, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("claude: list projects: %w", err)
	}
	sort.Slice(slugs, func(i, j int) bool { return slugs[i].Name() < slugs[j].Name() })

	var refs []importer.Ref
	for _, slug := range slugs {
		if !slug.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, slug.Name()))
		if err != nil {
			continue
		}
		sort.Slice(files, func(i, j int) bool { return files[i].Name() < files[j].Name() })

		// A session's subagent transcripts live in a directory named
		// after it, and that directory can outlive — or arrive without —
		// the session file itself: a transcript deleted or rotated away
		// leaves its subagents behind, and the two are written by
		// different code at different moments. So the directories are
		// collected first and struck off as their session file is seen;
		// whatever is left at the end is swept up on its own, with the
		// directory name standing in as the parent session id. Binding
		// the orphans to a parent nobody has ingested is fine — the
		// store leaves parent_id NULL and fills it in if the parent ever
		// turns up — and losing them is not, since a subagent transcript
		// is a whole conversation.
		orphans := map[string]bool{}
		for _, f := range files {
			if f.IsDir() && hasSubagents(root, slug.Name(), f.Name()) {
				orphans[f.Name()] = true
			}
		}
		for _, f := range files {
			if f.IsDir() || filepath.Ext(f.Name()) != ".jsonl" {
				continue
			}
			sessionID := strings.TrimSuffix(f.Name(), ".jsonl")
			delete(orphans, sessionID)
			refs = append(refs, importer.Ref{
				Provider: s.Name(),
				ID:       sessionID,
				Path:     filepath.Join(projectsDir, slug.Name(), f.Name()),
			})
			refs = append(refs, s.subagentRefs(root, slug.Name(), sessionID)...)
		}
		for _, sessionID := range sortedKeys(orphans) {
			refs = append(refs, s.subagentRefs(root, slug.Name(), sessionID)...)
		}
	}
	return refs, nil
}

// hasSubagents reports whether a session directory holds a subagents
// directory, which is what makes it worth sweeping rather than a stray
// directory somebody left in the projects tree.
func hasSubagents(root, slug, sessionID string) bool {
	info, err := os.Stat(filepath.Join(root, slug, sessionID, subagentsDir))
	return err == nil && info.IsDir()
}

// sortedKeys is the set as an ordered slice, so a sweep of orphaned
// subagent directories comes back in the same order on every run.
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// subagentRefs lists the transcripts of the agents one session dispatched.
// The agent id is taken from the filename rather than from the file, which
// keeps Discover from opening every transcript on the machine; Read then
// prefers the `agentId` the lines carry, so a filename that ever stops
// matching the field is the file's word against the directory's, and the
// file wins.
func (s Source) subagentRefs(root, slug, sessionID string) []importer.Ref {
	dir := filepath.Join(root, slug, sessionID, subagentsDir)
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name() < files[j].Name() })
	var refs []importer.Ref
	for _, f := range files {
		if f.IsDir() || filepath.Ext(f.Name()) != ".jsonl" {
			continue
		}
		stem := strings.TrimSuffix(f.Name(), ".jsonl")
		refs = append(refs, importer.Ref{
			Provider: s.Name(),
			ID:       strings.TrimPrefix(stem, agentFilePrefix),
			Path:     filepath.Join(projectsDir, slug, sessionID, subagentsDir, f.Name()),
		})
	}
	return refs
}
