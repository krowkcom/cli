package cursor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/krowkcom/cli/internal/harness"
	"github.com/krowkcom/cli/internal/importer"
)

// The names this package answers to. Provider is the model-vendor fallback
// written into session.provider and every message.provider — a message naming
// no vendor is filed under "cursor", because the harness drove the model
// whichever vendor ran it — and Harness is the tool that produced the
// transcript.
const (
	// Provider is the model vendor fallback.
	Provider = "cursor"
	// Harness is the tool that produced the transcript.
	Harness = "cursor"
)

// projectsDir is where Cursor keeps transcripts, relative to home. Joined
// under the home directory through importer.HomePath, so a symlink pointing
// out of home is refused rather than followed.
const projectsDir = ".cursor/projects"

// transcriptsDir is the directory under each project slug holding one
// directory per session.
const transcriptsDir = "agent-transcripts"

// Source is the Cursor importer. It holds nothing: every call takes the
// harness.Env it should resolve home through, so a test points it at a
// temporary directory rather than at the developer's own transcripts.
type Source struct{}

// Source really is one, checked at compile time rather than at the call site
// that first tries to use it as one.
var _ importer.Source = Source{}

// Name is the provider key, shared with the store's binding half and the
// import_state row key.
func (Source) Name() string { return importer.ProviderCursor }

// Discover lists every session transcript under the home directory.
//
// Sorted at both levels so two runs on an unchanged machine produce the same
// list, which is what makes a golden test of a discovery possible at all.
// Being cheap and slightly wrong is allowed here, as with the Claude reader:
// a machine with no Cursor on it has no transcripts (nil, nil), and a slug
// or session directory that cannot be read is skipped rather than failing a
// whole discovery over it.
func (s Source) Discover(env harness.Env) ([]importer.Ref, error) {
	if err := importer.CheckOS(); err != nil {
		return nil, fmt.Errorf("cursor: %w", err)
	}
	root, err := importer.HomePath(env, projectsDir)
	if err != nil {
		// errors.Is rather than os.IsNotExist: HomePath wraps with %w,
		// and the older predicate does not unwrap, so a machine with no
		// Cursor on it would surface as an error rather than as empty.
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("cursor: resolve projects directory: %w", err)
	}
	slugs, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("cursor: list projects: %w", err)
	}
	sort.Slice(slugs, func(i, j int) bool { return slugs[i].Name() < slugs[j].Name() })

	var refs []importer.Ref
	for _, slug := range slugs {
		if !slug.IsDir() {
			continue
		}
		// An unreadable slug directory is skipped, not fatal: a transcript
		// directory being replaced under the walk is ordinary and losing
		// every other session over it is not.
		transcripts, err := os.ReadDir(filepath.Join(root, slug.Name(), transcriptsDir))
		if err != nil {
			continue
		}
		sort.Slice(transcripts, func(i, j int) bool { return transcripts[i].Name() < transcripts[j].Name() })
		for _, sess := range transcripts {
			if !sess.IsDir() {
				continue
			}
			id := sess.Name()
			// The session file is named after its directory: <id>/<id>.jsonl.
			// A directory without it is a session with nothing written yet,
			// not a session — Discover names what Read can open. The leaf
			// must be a regular file: Read opens through O_NOFOLLOW and
			// refuses a symlinked leaf, so a symlink here would be a ref
			// Read fails on.
			name := id + ".jsonl"
			info, err := os.Lstat(filepath.Join(root, slug.Name(), transcriptsDir, id, name))
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			refs = append(refs, importer.Ref{
				Provider: s.Name(),
				ID:       id,
				Path:     filepath.Join(projectsDir, slug.Name(), transcriptsDir, id, name),
			})
		}
	}
	return refs, nil
}
