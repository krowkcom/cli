// Package claude reads Claude Code's transcripts off this machine and turns
// them into the canonical store.Thread that internal/importer fixes.
//
// Claude is the largest source on a developer's box and the one Phase 3
// exports from, so the thing that matters most here is that nothing is lost
// quietly. A JSONL transcript is not a list of messages with some noise
// around it: on a real machine roughly a third of the lines are
// `attachment`, and a further slice is `mode`, `last-prompt`,
// `queue-operation`, `atis-latch`, `pr-link`, `permission-mode`,
// `cost-state`, `file-history-snapshot`, `file-history-delta`, `ai-title`,
// `frame-link`, `continued-in` and `agent-name`. A reader that matched
// `user` and `assistant` and ignored the rest would drop most of the file
// and report a clean import. So every line lands in exactly one of four
// places — a message, a session event, Result.Classified, or Result.Skipped
// — and a test on the fixture asserts that those four add up to the line
// count with nothing left over.
//
// # The worktree comes from cwd, never from the directory name
//
// Claude files a session under a slug made by replacing every separator in
// the working directory with a dash: /home/elvinas/.buzz becomes
// `-home-elvinas--buzz`. That transform is not invertible — a dash in a real
// directory name is indistinguishable from a separator — so the slug is used
// for nothing but finding the file. The working directory comes from the
// `cwd` field the lines carry, and the worktree is the git toplevel found by
// walking up from it looking for a `.git` entry, with `vcs` reading `git`
// when one was found and `none` when the walk reached the root. A session run
// outside a checkout still gets a worktree row, at its own directory, because
// the store requires one and inventing a repository would be worse than
// admitting there is not one.
//
// # provider, harness and the two names for Claude
//
// There are two different questions with the same obvious answer, and they
// have different answers. `anthropic` is who ran the model; `claude` is what
// drove it. So Session.Provider and every Message.Provider say `anthropic`,
// Session.Harness and Binding.Harness say `claude`, and Binding.Provider says
// `claude` — the last one because it is not a description at all. It is half
// the UNIQUE key on session_binding and the prefix of the import_state row
// key, both of which are already written as importer.ProviderClaude by the
// contract this package is built against, and changing it would rename
// persisted rows to make a field read better.
//
// # Subagents are sessions
//
// A Task dispatch writes its own transcript to
// `<slug>/<sessionId>/subagents/agent-<agentId>.jsonl`. Those lines carry the
// parent's `sessionId` and their own `agentId`, so the child binds on the
// agent id and points at the parent through store.Thread.Parent. Discover
// returns a parent immediately before its own subagents, so a caller that
// ingests refs in order always has the parent row in place when the child
// arrives; a caller that does not gets a NULL parent_id and the next import
// fixes it.
//
// # A turn is a costing unit, not a link
//
// store.Message carries no turn_id from this importer, because the contract
// carries none: the writer inserts NULL and Thread has nowhere to say
// otherwise. Turns are still computed and still exact — the spans are
// positional over the same message list, so the mapping exists — but it
// lives in this package's arithmetic rather than in a column, which means a
// turn is what per-turn cost is summed over and nothing else. A reader that
// wants "which messages were in turn 3" cannot get it from the store yet,
// and should not be told it can by a column full of NULLs that looks like
// it could.
//
// # Read always reads from the start
//
// Read type-checks the cursor it is given and then ignores its offset. Turns
// are cumulative positional lists — the store maps turn i to seq i — and a
// turn's cost is summed over the assistant messages inside it, so a read that
// began halfway through a file could neither number its turns nor cost them.
// The alternative to re-reading is a partial turn list that renumbers on
// every import, which is the failure the contract's "turns are cumulative"
// note exists to prevent. Re-reading is affordable: a transcript is bounded
// at importer.DefaultMaxBytes, and the store dedups messages on
// (session_id, foreign_id), so the second read of a file inserts nothing.
// The returned cursor is still the honest watermark for the file, so a caller
// storing it loses nothing by doing so.
package claude
