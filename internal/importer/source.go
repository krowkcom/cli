package importer

import (
	"errors"
	"fmt"
	"strings"

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
	// Read parses ref from cursor onward. A zero or nil cursor in means
	// "from the start".
	//
	// The returned Cursor is the last safe watermark and is non-nil even
	// when err is non-nil: a read that failed halfway still knows how far
	// it got safely. The one exception is a rejected cursor —
	// ErrCursorType below — where Read hands back the cursor it was given,
	// untouched, because it never read anything and has nothing of its own
	// to say.
	//
	// `krowk sessions import`, the only caller, stores none of those
	// cursors: a failed Read returns no usable Thread, and a watermark
	// moved past rows that were never ingested is a gap in the store that
	// no later run would ever fill. Advancing the cursor without the rows
	// is the one failure that is silent and permanent, and re-reading a
	// prefix is only work. Sources still return the watermark because it
	// is honest and a future caller may have somewhere to put it; nothing
	// today does.
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

// The bounds on what a Result remembers. A Result is held for the whole of
// one Read, so anything it accumulates per line is memory the importer pays
// for per line — and the pathological input is not exotic: a 64 MiB file of
// short non-JSON lines is 33 million skips, and a reason string can be a
// callback's error carrying a whole 16 MiB line inside it.
const (
	// maxSkippedRetained is how many skipped lines are kept in detail. The
	// first hundred answer "which lines?" as well as thirty-three million
	// would; the count answers "how many?" on its own.
	maxSkippedRetained = 100
	// maxSkipReasonBytes bounds one reason. A reason is a sentence, and
	// anything longer is a caller having embedded the line in its error.
	maxSkipReasonBytes = 256
)

// SkippedLine is one line that did not parse, kept rather than merely
// counted so that "12 skipped" can be answered with which twelve. Line is
// 1-based within the read that produced it — a resumed read starts counting
// at its own first line, because numbering from the top of the file would
// mean re-scanning everything the cursor exists to skip. Offset is the
// absolute byte offset of the line's first byte, which is unambiguous either
// way. Reason is bounded by maxSkipReasonBytes, ellipsis included.
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
	// SkippedCount is how many lines could not be used. It is the honest
	// total and always exact.
	SkippedCount int
	// Skipped is the first maxSkippedRetained of those lines in detail.
	// It is a sample, not the total — SkippedCount is the total — because
	// a Result that grew a struct per bad line would turn a corrupt file
	// into an out-of-memory failure, which is a far worse way to report a
	// corrupt file than a number.
	Skipped []SkippedLine
}

// Skip records a line that could not be used, with why. Every call counts;
// only the first maxSkippedRetained are described.
func (r *Result) Skip(line int, offset int64, reason string) {
	r.SkippedCount++
	if len(r.Skipped) >= maxSkippedRetained {
		return
	}
	r.Skipped = append(r.Skipped, SkippedLine{Line: line, Offset: offset, Reason: truncateReason(reason)})
}

// truncateReason bounds a reason string. The cut is made valid again rather
// than left as it fell: slicing bytes can land inside a rune, and these
// strings end up in a database column and on a terminal.
func truncateReason(reason string) string {
	const ellipsis = "…"
	if len(reason) <= maxSkipReasonBytes {
		return reason
	}
	return strings.ToValidUTF8(reason[:maxSkipReasonBytes-len(ellipsis)], "") + ellipsis
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
	r.SkippedCount += other.SkippedCount
	for _, sl := range other.Skipped {
		if len(r.Skipped) >= maxSkippedRetained {
			break
		}
		r.Skipped = append(r.Skipped, sl)
	}
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
