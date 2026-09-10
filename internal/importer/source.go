package importer

import (
	"errors"
	"fmt"

	"github.com/krowkcom/cli/internal/harness"
	"github.com/krowkcom/cli/internal/store"
)

// The provider names a Source may answer to. They are the same strings that
// land in session.provider and in the import_state key, so a rename here is
// a rename of persisted rows — which is why they are constants and not
// literals scattered through three packages.
const (
	ProviderClaude   = "claude"
	ProviderCursor   = "cursor"
	ProviderOpencode = "opencode"
)

// Source is one agent's transcripts on this machine. Discover says what is
// there, Read turns one of those things into the canonical Thread and hands
// back the watermark to resume from.
//
// The split matters: Discover is allowed to be cheap and wrong-ish (a file
// that vanishes before Read is not an error worth failing a whole import
// over), Read is the part that must be exact. Neither takes a context yet —
// both are local filesystem work bounded by MaxBytes, and there is nothing
// to cancel that a killed process does not already end.
//
// Read also reports a Result. It is not decoration: a transcript with three
// unreadable lines is a successful read of a slightly lossy file, and the
// only honest way to say that is to return the thread and the count of what
// did not make it.
type Source interface {
	// Name is the provider string, one of the Provider* constants.
	Name() string
	// Discover lists the transcripts this source can see through env. On an
	// unsupported OS it returns ErrUnsupportedOS and nothing else.
	Discover(env harness.Env) ([]Ref, error)
	// Read parses ref from cursor onward. The returned Cursor is where a
	// later call should resume; a zero cursor in means "from the start".
	//
	// A Source must return ErrCursorType when cursor is not the concrete
	// kind it takes. Silently falling back to a full rescan would be the
	// tempting alternative and is the wrong one: it turns a caller's bug
	// into an intermittent performance problem nobody can find.
	Read(env harness.Env, ref Ref, cursor Cursor) (store.Thread, Cursor, Result, error)
}

// Ref names one thing to read, in terms every source shares. ID is the
// source's own identifier for it — a Claude session uuid, an opencode
// session id — and is what the import_state key is built from, so it must
// survive the file moving. Path is where it was found, which may be a file
// or a directory depending on the source and is a hint for Read, never the
// identity.
type Ref struct {
	Provider string
	ID       string
	Path     string
}

// Key is the import_state.source value for this ref: "<provider>:<ref>".
// Falling back to Path when ID is empty keeps a source that has no stable
// id from writing every cursor to the same row; it is a worse key, because
// a moved file rescans, but it is a key.
func (r Ref) Key() string {
	id := r.ID
	if id == "" {
		id = r.Path
	}
	return r.Provider + ":" + id
}

// SkippedLine is one line that did not parse, kept rather than counted so
// that "12 skipped" can be answered with which twelve. Line is 1-based
// within the read that produced it — a resumed read starts counting at its
// own first line, because numbering from the top of the file would mean
// re-scanning everything the cursor exists to skip. Offset is the absolute
// byte offset of the line's first byte, which is unambiguous either way.
type SkippedLine struct {
	Line   int
	Offset int64
	Reason string
}

// Result is what one Read did, in rows rather than in prose. It exists so a
// lossy import is reportable instead of either fatal or invisible.
type Result struct {
	// Lines is every non-blank line the reader consumed, skipped ones
	// included. Blank lines are padding and are not counted: a Lines total
	// inflated by whitespace would make the skip ratio meaningless.
	Lines int
	// Unknown counts parts whose source type was not in the fixed set and
	// so landed as `unknown`.
	Unknown int
	// UnknownTypes is how many of Unknown each raw type accounted for — the
	// list you would want when adding the type that got missed.
	UnknownTypes map[string]int
	// Classified counts lines a source recognised but deliberately did not
	// turn into a message: Claude's `mode`, `file-history-snapshot`,
	// `last-prompt` and `summary` lines are transcript furniture, not
	// transcript.
	Classified map[string]int
	// Skipped is every line that could not be parsed at all.
	Skipped []SkippedLine
}

// Skip records a line that could not be used, with why.
func (r *Result) Skip(line int, offset int64, reason string) {
	r.Skipped = append(r.Skipped, SkippedLine{Line: line, Offset: offset, Reason: reason})
}

// Classify records a line the source understood and chose not to import.
func (r *Result) Classify(kind string) {
	if r.Classified == nil {
		r.Classified = map[string]int{}
	}
	r.Classified[kind]++
}

// Merge folds other into r, for a Read that walks more than one file.
func (r *Result) Merge(other Result) {
	r.Lines += other.Lines
	r.Unknown += other.Unknown
	for k, v := range other.UnknownTypes {
		if r.UnknownTypes == nil {
			r.UnknownTypes = map[string]int{}
		}
		r.UnknownTypes[k] += v
	}
	for k, v := range other.Classified {
		if r.Classified == nil {
			r.Classified = map[string]int{}
		}
		r.Classified[k] += v
	}
	r.Skipped = append(r.Skipped, other.Skipped...)
}

// ErrUnsupportedOS is what Discover returns where krowk cannot find
// transcripts at all. Windows is the case today: the paths, the file locking
// and the home resolution all differ enough that a half-working importer
// would report an empty machine rather than an unsupported one, and an empty
// machine is the answer a user would believe.
var ErrUnsupportedOS = errors.New("importing is not supported on this operating system")

// ErrCursorType is a cursor of the wrong concrete kind for the Source it was
// handed to — a SQLiteCursor given to a JSONL source, or a cursor type from
// a later build. It is a refusal rather than a rescan so the mistake surfaces
// where it was made.
var ErrCursorType = errors.New("cursor is not the kind this source takes")

// CheckOS is the first line of every Discover implementation. Keeping the
// refusal in one build-tagged function is what lets the sources stay
// tag-free and still compile under GOOS=windows.
func CheckOS() error { return checkOS() }

// unsupportedOS wraps ErrUnsupportedOS with the provider that refused, so a
// caller aggregating three sources can say which.
func unsupportedOS(provider string) error {
	return fmt.Errorf("%s: %w", provider, ErrUnsupportedOS)
}
