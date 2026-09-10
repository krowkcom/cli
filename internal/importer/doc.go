// Package importer is the contract three importers have to agree on before
// any of them is written. Claude reads JSONL under a home directory, cursor
// reads a SQLite file, opencode reads its own storage — and if each one
// decides for itself what a part is, how far it has read and which files it
// is allowed to open, then Phase 3 does not have one exporter with three
// front ends, it has three exporters. So the vocabulary lands here first and
// the sources land against it.
//
// Four things are fixed here and nowhere else:
//
// The part types. store.Part.Type is an open string so a new content block
// cannot need a schema change, but open at the schema is not open at the
// importer: a source that invents `reasoning` where another says `thinking`
// makes every reader downstream carry a synonym table. The set is closed by
// KnownPartType, and anything a source does not recognise becomes `unknown`
// carrying its raw payload — visible, counted, and never silently dropped.
//
// The watermark. It is a byte offset into a named file, not a modification
// time: mtime is a lie on a copied checkout and a coarse lie everywhere
// else, and a file that got shorter since the last read is not a file that
// grew. Cursors carry the size they were taken at for exactly that reason —
// a shrink is a rescan, not an append.
//
// The read. A home directory is the user's; a checkout is whoever wrote it.
// That is the same rule internal/harness applies to configs, and this
// package applies it to transcripts, in its own code rather than by reaching
// into that package: OpenHome resolves under harness.HomeDir, refuses to
// leave it, and refuses to be led out of it by a symlink into a repository.
//
// The turn boundary. A turn starts at a real user prompt. The transcripts
// are full of user-role lines that are not prompts — hook output, attached
// files, tool results wearing the user role — and a reader that treats them
// as prompts reports ten turns where a person had one.
//
// Nothing here reads the process environment. Every entry point takes an
// harness.Env, so a test hands it a temporary home instead of the
// developer's own.
package importer
