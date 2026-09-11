package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// ingestBatchSize caps how many messages one Ingest transaction holds.
// Short transactions are the concurrency contract: a concurrent krowk push
// waits on busy_timeout (10s, armed by Open) instead of meeting a lock held
// for a whole import, so the writer must never widen a transaction to fit.
const ingestBatchSize = 500

// Count is rows inserted vs rows skipped for one table in one Ingest.
// Skipped means the row was already there: an upserted worktree, a known
// binding, a foreign_id seen before, or a position-keyed prefix re-sent.
type Count struct {
	Inserted int
	Skipped  int
}

// Result reports one Ingest call per table. A re-ingest of the same Thread
// shows 0 Inserted on every table.
type Result struct {
	Worktrees Count
	Sessions  Count
	Bindings  Count
	Turns     Count
	Messages  Count
	Parts     Count
	Events    Count
}

// Writer ingests canonical Threads into an opened store. It holds the
// database handle and the clock — never the environment: Open resolves the
// path, so nothing here consults process variables or the real home, and
// every id it mints and every time_* it writes comes from the injected
// clock.
type Writer struct {
	db     *sql.DB
	clock  Clock
	minter *Minter
}

// NewWriter returns a Writer over db, which must come from Open. A nil
// clock means time.Now; tests pass a frozen clock so ids and time columns
// agree.
func NewWriter(db *sql.DB, clock Clock) *Writer {
	if clock == nil {
		clock = defaultMinter.clock
	}
	return &Writer{db: db, clock: clock, minter: NewMinter(clock)}
}

// isConstraintViolation reports whether err is a SQLite uniqueness
// failure. Only that class triggers the adopt-the-winner paths: a disk
// error or a CHECK failure must surface, never be mistaken for a
// concurrent writer.
func isConstraintViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// isRetryable reports whether err is worth one more Ingest attempt against
// fresh state: a uniqueness conflict with a concurrent writer, or a lock
// the writer would not wait out. The lock case is the DEFERRED-transaction
// upgrade window — both writers read, one commits, the other's upgrade
// fails as SQLITE_BUSY_SNAPSHOT without invoking the busy timeout — so a
// re-run that re-reads converges instead of surfacing "database is locked".
// Anything else (disk, CHECK, context) is returned at once.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// "is locked" covers all three SQLite lock texts: "database is
	// locked" (BUSY), "database table is locked" (LOCKED) and "database
	// schema is locked". Ingest never alters the schema, so any of them
	// means a concurrent writer, never a DDL conflict of its own.
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "is locked")
}

// validRole reports whether r is in the message CHECK set.
func validRole(r Role) bool {
	switch r {
	case RoleUser, RoleAssistant, RoleSystem, RoleTool, RoleError:
		return true
	}
	return false
}

// Ingest writes th into the store and reports what it inserted vs skipped:
//
//   - worktree upserted by path;
//   - session found by (provider, foreign_session_id) binding, else created
//     with its binding (a lost create race adopts the winner);
//   - session.parent_id set from th.Parent's binding when that binding is
//     already in the store and the column is still NULL — a parent nobody
//     has ingested yet leaves it NULL, and a later Ingest of the same
//     child fills it in;
//   - messages with a ForeignID already in the session skipped with their
//     parts, the rest appended with seq after the current max;
//   - turns and events treated as cumulative positional lists: position i
//     is seq i, so a re-sent prefix is skipped and only the tail inserts —
//     callers must still hold a cursor and never re-send with changed
//     content, because the kept prefix wins silently. The one exception is
//     the last stored turn, whose status and costs are refreshed from the
//     re-sent list; see insertTurnTail for why that row and no other;
//   - messages with no ForeignID always appended, so their callers must
//     hold a cursor and never re-send at all.
//
// Messages go in chunks of ingestBatchSize per transaction; worktree,
// session, binding, turns and events go in one short transaction first.
// Validation runs before any write, so a bad role fails with the store
// untouched. A mid-ingest failure may leave committed prefix transactions
// behind, but a retry converges: every step skips what is already there,
// which is also what makes the whole-call retry below safe — a uniqueness
// conflict or a lock-upgrade loss against a concurrent writer re-runs
// against fresh state instead of surfacing, up to three attempts. A
// retried call merges per-attempt counts, so under contention one row can
// appear both inserted (first attempt wrote it) and skipped (the retry
// re-observed it) — the exact success-path counts the tests pin never
// retry, so they stay exact.
func (w *Writer) Ingest(ctx context.Context, th Thread) (Result, error) {
	var res Result
	if th.Worktree.Path == "" {
		return res, fmt.Errorf("store: ingest needs a worktree path")
	}
	if th.Binding.ForeignSessionID == "" {
		return res, fmt.Errorf("store: ingest needs a binding foreign_session_id")
	}
	for i, m := range th.Messages {
		if !validRole(m.Role) {
			return res, fmt.Errorf("store: message %d has role %q, want user|assistant|system|tool|error", i, m.Role)
		}
	}

	now := w.minter.NowMS()
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			// Stagger the re-run: without a pause two losers retry in
			// lockstep and collide again. Same shape as busyRetry — the
			// lock holder's transactions are short, so milliseconds
			// suffice — plus a nanosecond jitter so two processes that
			// failed together do not wake together. (The jitter reads
			// the wall clock, not the injected one: it sets no stored
			// value.) Honors cancellation while waiting.
			jitter := time.Duration(time.Now().UnixNano()%25) * time.Millisecond
			select {
			case <-ctx.Done():
				return res, ctx.Err()
			case <-time.After(time.Duration(attempt)*25*time.Millisecond + jitter):
			}
		}
		var r Result
		r, err = w.ingestOnce(ctx, th, now)
		res.Worktrees.Inserted += r.Worktrees.Inserted
		res.Worktrees.Skipped += r.Worktrees.Skipped
		res.Sessions.Inserted += r.Sessions.Inserted
		res.Sessions.Skipped += r.Sessions.Skipped
		res.Bindings.Inserted += r.Bindings.Inserted
		res.Bindings.Skipped += r.Bindings.Skipped
		res.Turns.Inserted += r.Turns.Inserted
		res.Turns.Skipped += r.Turns.Skipped
		res.Messages.Inserted += r.Messages.Inserted
		res.Messages.Skipped += r.Messages.Skipped
		res.Parts.Inserted += r.Parts.Inserted
		res.Parts.Skipped += r.Parts.Skipped
		res.Events.Inserted += r.Events.Inserted
		res.Events.Skipped += r.Events.Skipped
		if err == nil || !isRetryable(err) {
			return res, err
		}
	}
	return res, err
}

// ingestOnce is one Ingest attempt: short transactions that each roll back
// on failure, so a uniqueness conflict with a concurrent writer leaves the
// store in a state the next attempt converges on. Counts from the first
// transaction are reported only once it commits: anything earlier returns
// zero, so a rolled-back attempt never contributes phantom Inserted rows.
// Message-chunk counts are commit-local for the same reason, and only
// committed chunks reach the total.
func (w *Writer) ingestOnce(ctx context.Context, th Thread, now int64) (Result, error) {
	var res Result

	// Title fallback: a thread naming no title is listed by the first 80
	// chars of its first user text, computed at import into session.title
	// so the listing never reads message/part blobs to name a row.
	if th.Session.Title == "" {
		th.Session.Title = sessionTitleFallback(th.Messages)
	}

	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("store: ingest begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			tx.Rollback()
		}
	}()

	worktreeID, inserted, err := upsertWorktree(ctx, tx, w.minter, now, th.Worktree)
	if err != nil {
		return Result{}, err
	}
	if inserted {
		res.Worktrees.Inserted++
	} else {
		res.Worktrees.Skipped++
	}

	sessionID, sessIns, bindIns, err := findOrCreateSession(ctx, tx, w.minter, now, worktreeID, th.Session, th.Binding)
	if err != nil {
		return Result{}, err
	}
	if sessIns {
		res.Sessions.Inserted++
	} else {
		res.Sessions.Skipped++
	}
	if bindIns {
		res.Bindings.Inserted++
	} else {
		res.Bindings.Skipped++
	}

	if err := linkParent(ctx, tx, sessionID, th.Parent); err != nil {
		return Result{}, err
	}

	ti, ts, err := insertTurnTail(ctx, tx, w.minter, now, sessionID, th.Turns)
	if err != nil {
		return Result{}, err
	}
	res.Turns.Inserted += ti
	res.Turns.Skipped += ts

	ei, es, err := insertEventTail(ctx, tx, w.minter, now, sessionID, th.Events)
	if err != nil {
		return Result{}, err
	}
	res.Events.Inserted += ei
	res.Events.Skipped += es

	if err := tx.Commit(); err != nil {
		return Result{}, fmt.Errorf("store: ingest commit: %w", err)
	}
	committed = true

	mi, ms, pi, ps, err := w.insertMessages(ctx, sessionID, now, th.Messages)
	if err != nil {
		return res, err
	}
	res.Messages.Inserted += mi
	res.Messages.Skipped += ms
	res.Parts.Inserted += pi
	res.Parts.Skipped += ps

	return res, nil
}

// upsertWorktree finds the worktree by path or creates it, refreshing the
// display fields on a hit. It reports whether it inserted. A lost insert
// race re-reads the winner instead of erroring.
func upsertWorktree(ctx context.Context, tx *sql.Tx, m *Minter, now int64, wt Worktree) (string, bool, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM worktree WHERE path = ?`, wt.Path).Scan(&id)
	if err == nil {
		if _, err := tx.ExecContext(ctx,
			`UPDATE worktree SET vcs = ?, name = ?, time_updated = ? WHERE id = ?`,
			wt.VCS, wt.Name, now, id); err != nil {
			return "", false, fmt.Errorf("store: ingest update worktree: %w", err)
		}
		return id, false, nil
	}
	if err != sql.ErrNoRows {
		return "", false, fmt.Errorf("store: ingest find worktree: %w", err)
	}
	id = m.NewID()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO worktree (id, path, vcs, name, time_created, time_updated) VALUES (?, ?, ?, ?, ?, ?)`,
		id, wt.Path, wt.VCS, wt.Name, now, now); err != nil {
		// A concurrent Ingest inserted the same path first: adopt it. Any
		// other failure (disk, CHECK) is returned, never mistaken for a
		// race.
		if !isConstraintViolation(err) {
			return "", false, fmt.Errorf("store: ingest insert worktree: %w", err)
		}
		var winner string
		if qerr := tx.QueryRowContext(ctx, `SELECT id FROM worktree WHERE path = ?`, wt.Path).Scan(&winner); qerr == nil {
			if _, uerr := tx.ExecContext(ctx,
				`UPDATE worktree SET vcs = ?, name = ?, time_updated = ? WHERE id = ?`,
				wt.VCS, wt.Name, now, winner); uerr != nil {
				return "", false, fmt.Errorf("store: ingest update worktree: %w", uerr)
			}
			return winner, false, nil
		}
		return "", false, fmt.Errorf("store: ingest insert worktree: %w", err)
	}
	return id, true, nil
}

// findOrCreateSession resolves the session through the binding key and
// creates session plus binding when neither exists. Reports whether each
// row was inserted.
//
// A session found by its binding has its worktree re-pointed as well as
// its display fields refreshed. That is not merely tidiness: an importer
// that could not tell where a session ran the first time — the Claude
// reader falls back to the transcript's own directory when no line carried
// a cwd — would otherwise be stuck with that placeholder forever, even
// once a later read of the same transcript found the real checkout. The
// worktree row itself is upserted by path and never deleted, so
// re-pointing a session moves the session and leaves the old worktree
// standing. A lost binding race adopts the winner's session and
// refreshes its display fields, so two Threads naming the same
// (provider, foreign_session_id) converge on one session row and one
// binding row.
func findOrCreateSession(ctx context.Context, tx *sql.Tx, m *Minter, now int64, worktreeID string, s Session, b Binding) (string, bool, bool, error) {
	var sessionID string
	err := tx.QueryRowContext(ctx,
		`SELECT session_id FROM session_binding WHERE provider = ? AND foreign_session_id = ?`,
		b.Provider, b.ForeignSessionID).Scan(&sessionID)
	if err == nil {
		title := s.Title
		if title == "" {
			// Never wipe a stored title with an untitled re-import. A
			// stored untitled row backfills here when the re-import
			// carries user text; a row no re-import names stays untitled
			// — there is no migration rewriting stored titles.
			if terr := tx.QueryRowContext(ctx, `SELECT title FROM session WHERE id = ?`, sessionID).Scan(&title); terr != nil {
				return "", false, false, fmt.Errorf("store: ingest read session title: %w", terr)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE session SET worktree_id = ?, directory = ?, title = ?, model = ?, provider = ?, harness = ?, time_updated = ? WHERE id = ?`,
			worktreeID, s.Directory, title, s.Model, s.Provider, s.Harness, now, sessionID); err != nil {
			return "", false, false, fmt.Errorf("store: ingest update session: %w", err)
		}
		return sessionID, false, false, nil
	}
	if err != sql.ErrNoRows {
		return "", false, false, fmt.Errorf("store: ingest find binding: %w", err)
	}
	sessionID = m.NewID()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO session (id, worktree_id, directory, title, model, provider, harness, time_created, time_updated) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, worktreeID, s.Directory, s.Title, s.Model, s.Provider, s.Harness, now, now); err != nil {
		return "", false, false, fmt.Errorf("store: ingest insert session: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO session_binding (id, session_id, provider, harness, foreign_session_id, resume_cmd, time_created, time_updated) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		m.NewID(), sessionID, b.Provider, b.Harness, b.ForeignSessionID, b.ResumeCmd, now, now); err != nil {
		// A concurrent Ingest bound the same foreign session first: drop
		// this session row inside the same transaction and adopt the
		// winner, so the key still names exactly one session. Any other
		// failure is returned, never mistaken for a race.
		if !isConstraintViolation(err) {
			return "", false, false, fmt.Errorf("store: ingest insert binding: %w", err)
		}
		var winner string
		if qerr := tx.QueryRowContext(ctx,
			`SELECT session_id FROM session_binding WHERE provider = ? AND foreign_session_id = ?`,
			b.Provider, b.ForeignSessionID).Scan(&winner); qerr == nil {
			if _, derr := tx.ExecContext(ctx, `DELETE FROM session WHERE id = ?`, sessionID); derr != nil {
				return "", false, false, fmt.Errorf("store: ingest adopt session: %w", derr)
			}
			winnerTitle, uerr := storedTitleOr(ctx, tx, winner, s.Title)
			if uerr != nil {
				return "", false, false, uerr
			}
			if _, uerr := tx.ExecContext(ctx,
				`UPDATE session SET worktree_id = ?, directory = ?, title = ?, model = ?, provider = ?, harness = ?, time_updated = ? WHERE id = ?`,
				worktreeID, s.Directory, winnerTitle, s.Model, s.Provider, s.Harness, now, winner); uerr != nil {
				return "", false, false, fmt.Errorf("store: ingest update session: %w", uerr)
			}
			return winner, false, false, nil
		}
		return "", false, false, fmt.Errorf("store: ingest insert binding: %w", err)
	}
	return sessionID, true, true, nil
}

// storedTitleOr resolves the title the binding-race path writes: the
// winner's stored title when it has one, else this attempt's fallback, so
// an untitled loser never wipes the title the winner just stored.
func storedTitleOr(ctx context.Context, tx *sql.Tx, sessionID, incoming string) (string, error) {
	var stored string
	if err := tx.QueryRowContext(ctx, `SELECT title FROM session WHERE id = ?`, sessionID).Scan(&stored); err != nil {
		return "", fmt.Errorf("store: ingest read session title: %w", err)
	}
	if stored != "" {
		return stored, nil
	}
	return incoming, nil
}

// linkParent points sessionID at the session its parent binding names, when
// there is one to point at.
//
// Three things are deliberate here. A parent binding nobody has ingested yet
// is not an error: a Claude subagent transcript is a file of its own, and a
// caller importing files in whatever order the filesystem handed them back
// would otherwise fail on every child that arrived early. It is left NULL and
// a later Ingest of the same child — which happens on the next import, since
// the child's messages dedup but its session row is always revisited — fills
// it in.
//
// The UPDATE only touches a NULL column. Re-parenting a session that already
// has a parent would be the store silently rewriting history on the strength
// of one importer's reading of one file, and a wrong parent is harder to
// notice than a missing one.
//
// A session is never its own parent. The guard is cheap and the alternative
// is a row the parent_id foreign key happily accepts and every tree walk
// downstream loops on.
func linkParent(ctx context.Context, tx *sql.Tx, sessionID string, parent *Binding) error {
	if parent == nil || parent.ForeignSessionID == "" {
		return nil
	}
	var parentID string
	err := tx.QueryRowContext(ctx,
		`SELECT session_id FROM session_binding WHERE provider = ? AND foreign_session_id = ?`,
		parent.Provider, parent.ForeignSessionID).Scan(&parentID)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: ingest find parent binding: %w", err)
	}
	if parentID == sessionID {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE session SET parent_id = ? WHERE id = ? AND parent_id IS NULL`,
		parentID, sessionID); err != nil {
		return fmt.Errorf("store: ingest set parent: %w", err)
	}
	return nil
}

// insertTurnTail appends the turns past the stored prefix: position i is
// seq i, so the first len(stored) entries are the known prefix and only
// the tail inserts. Reports inserted vs skipped — the refreshed last turn
// counts as skipped, because it is a row that was already there. The next seq comes from
// MAX, not COUNT, so it stays correct even if the table ever held a gap.
func insertTurnTail(ctx context.Context, tx *sql.Tx, m *Minter, now int64, sessionID string, turns []Turn) (int, int, error) {
	var have, maxSeq int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(seq), -1) FROM turn WHERE session_id = ?`, sessionID).Scan(&have, &maxSeq); err != nil {
		return 0, 0, fmt.Errorf("store: ingest count turns: %w", err)
	}
	skipped := len(turns)
	if skipped > have {
		skipped = have
	}
	// The last stored turn is the only prefix row allowed to change, and
	// it has to be allowed to: see refreshLastTurn. The guard on
	// have == maxSeq+1 is what keeps the pairing honest — position i is
	// seq i only while the seqs run 0..have-1 without a gap, and pairing
	// the row at MAX(seq) with turns[have-1] across a gap would refresh
	// one turn from another turn's costs. A gapped table refreshes
	// nothing and still appends correctly.
	if have > 0 && have == maxSeq+1 && len(turns) >= have {
		if err := refreshLastTurn(ctx, tx, now, sessionID, maxSeq, turns[have-1]); err != nil {
			return 0, skipped, err
		}
	}
	inserted := 0
	for i, t := range turns[skipped:] {
		var usd any
		if t.CostUSDMicros != nil {
			usd = *t.CostUSDMicros
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO turn (id, session_id, seq, status, cost_input_tokens, cost_output_tokens, cost_total_tokens, cost_cache_read_tokens, cost_cache_write_tokens, cost_reasoning_tokens, cost_usd_micros, time_created, time_updated) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.NewID(), sessionID, maxSeq+1+i, t.Status, t.CostInput, t.CostOutput, t.CostTotal, t.CostCacheRead, t.CostCacheWrite, t.CostReasoning, usd, now, now); err != nil {
			return inserted, skipped, fmt.Errorf("store: ingest insert turn: %w", err)
		}
		inserted++
	}
	return inserted, skipped, nil
}

// refreshLastTurn rewrites the status and costs of the turn at seq from the
// re-sent list.
//
// This is the one place the store lets a positional prefix change, and the
// reason is that the alternative is silently wrong on every live session.
// An importer reads a transcript while somebody is still using it, so the
// last turn it sees is a turn in flight: the prompt has landed, two of its
// eventual twelve API calls have happened, and the cost the store is handed
// is a fraction of what that turn will end up costing. The importer re-reads
// the whole file next time and hands over the finished figure — and a
// prefix skipped verbatim would keep the fraction forever. The transcript is
// not wrong and the store is not wrong; the row is just old, and nothing
// would ever correct it.
//
// It is safe for exactly one row because of what closes a turn: a turn ends
// when the next one opens, so every turn but the last is already final by
// the time a later turn exists to follow it. Refreshing further back would
// be the store rewriting settled history on one importer's re-reading of one
// file, which is the thing the positional-prefix rule is there to prevent.
//
// Costs are overwritten rather than added to. The incoming list is
// cumulative — it is the whole turn as read from the whole file, not a delta
// — so summing would double the part already stored. Which is also the one
// way to misuse this: ingesting a Thread from a Read that failed partway,
// whose turn list happens to be as long as the stored one, rewrites the
// last turn downward with the truncated figure, so a caller must not ingest
// a thread whose Read returned an error.
func refreshLastTurn(ctx context.Context, tx *sql.Tx, now int64, sessionID string, seq int, t Turn) error {
	var usd any
	if t.CostUSDMicros != nil {
		usd = *t.CostUSDMicros
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE turn SET status = ?, cost_input_tokens = ?, cost_output_tokens = ?, cost_total_tokens = ?, cost_cache_read_tokens = ?, cost_cache_write_tokens = ?, cost_reasoning_tokens = ?, cost_usd_micros = ?, time_updated = ? WHERE session_id = ? AND seq = ?`,
		t.Status, t.CostInput, t.CostOutput, t.CostTotal, t.CostCacheRead, t.CostCacheWrite, t.CostReasoning, usd, now, sessionID, seq); err != nil {
		return fmt.Errorf("store: ingest refresh turn: %w", err)
	}
	return nil
}

// insertEventTail is insertTurnTail for session events: position i is
// seq i, the stored prefix is skipped, the tail inserts.
func insertEventTail(ctx context.Context, tx *sql.Tx, m *Minter, now int64, sessionID string, events []Event) (int, int, error) {
	var have, maxSeq int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(seq), -1) FROM session_event WHERE session_id = ?`, sessionID).Scan(&have, &maxSeq); err != nil {
		return 0, 0, fmt.Errorf("store: ingest count events: %w", err)
	}
	skipped := len(events)
	if skipped > have {
		skipped = have
	}
	inserted := 0
	for i, e := range events[skipped:] {
		data := e.Data
		if data == "" {
			data = "{}"
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO session_event (id, session_id, seq, type, data, time_created) VALUES (?, ?, ?, ?, ?, ?)`,
			m.NewID(), sessionID, maxSeq+1+i, e.Type, data, now); err != nil {
			return inserted, skipped, fmt.Errorf("store: ingest insert event: %w", err)
		}
		inserted++
	}
	return inserted, skipped, nil
}

// insertMessages appends the messages whose ForeignID is new to the
// session, in Thread order, with seq after the current max, in chunks of
// ingestBatchSize per transaction. Parts ride the same transaction as
// their message with per-message seq. Reports message and part counts.
func (w *Writer) insertMessages(ctx context.Context, sessionID string, now int64, msgs []Message) (mi, ms, pi, ps int, err error) {
	known := map[string]bool{}
	rows, err := w.db.QueryContext(ctx, `SELECT foreign_id FROM message WHERE session_id = ? AND foreign_id IS NOT NULL`, sessionID)
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("store: ingest list foreign ids: %w", err)
	}
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			rows.Close()
			return 0, 0, 0, 0, fmt.Errorf("store: ingest scan foreign ids: %w", err)
		}
		known[f] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("store: ingest list foreign ids: %w", err)
	}

	var maxSeq int
	if err := w.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), -1) FROM message WHERE session_id = ?`, sessionID).Scan(&maxSeq); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("store: ingest max message seq: %w", err)
	}
	nextSeq := maxSeq + 1

	// Filter first so chunk boundaries never split a skip decision: new
	// holds exactly the messages this call will append, in order.
	type pending struct {
		msg Message
		seq int
	}
	var new []pending
	for _, msg := range msgs {
		if msg.ForeignID != "" && known[msg.ForeignID] {
			ms++
			ps += len(msg.Parts)
			continue
		}
		if msg.ForeignID != "" {
			known[msg.ForeignID] = true
		}
		new = append(new, pending{msg: msg, seq: nextSeq})
		nextSeq++
	}

	for start := 0; start < len(new); start += ingestBatchSize {
		end := start + ingestBatchSize
		if end > len(new) {
			end = len(new)
		}
		chunk := new[start:end]
		tx, err := w.db.BeginTx(ctx, nil)
		if err != nil {
			return mi, ms, pi, ps, fmt.Errorf("store: ingest begin: %w", err)
		}
		// Rollback on every failure below; the Commit past them reports
		// its own error, so a failed commit fails here, not on retry.
		// No defer: this runs per chunk, and a stacked rollback after a
		// commit would only report ErrTxDone noise. Counts stay local
		// until the commit lands, so a failed chunk reports nothing for
		// rows it did not write.
		var cmi, cpi int
		for _, p := range chunk {
			nparts, ferr := insertOneMessage(ctx, tx, w.minter, now, sessionID, p.msg, p.seq)
			if ferr != nil {
				tx.Rollback()
				return mi, ms, pi, ps, ferr
			}
			cmi++
			cpi += nparts
		}
		if err := tx.Commit(); err != nil {
			tx.Rollback()
			return mi, ms, pi, ps, fmt.Errorf("store: ingest commit: %w", err)
		}
		mi += cmi
		pi += cpi
	}
	return mi, ms, pi, ps, nil
}

// insertOneMessage writes one message row plus its parts and returns the
// part count. An empty ForeignID, ToolCallID or Signature stores NULL;
// empty Usage or Data stores '{}'.
func insertOneMessage(ctx context.Context, tx *sql.Tx, m *Minter, now int64, sessionID string, msg Message, seq int) (int, error) {
	var foreign any
	if msg.ForeignID != "" {
		foreign = msg.ForeignID
	}
	usage := msg.Usage
	if usage == "" {
		usage = "{}"
	}
	var raw any
	if msg.RawJSON != nil {
		raw = *msg.RawJSON
	}
	msgID := m.NewID()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO message (id, session_id, turn_id, seq, role, provider, model, foreign_id, usage, raw_json, time_created) VALUES (?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, ?)`,
		msgID, sessionID, seq, string(msg.Role), msg.Provider, msg.Model, foreign, usage, raw, now); err != nil {
		return 0, fmt.Errorf("store: ingest insert message: %w", err)
	}
	for i, p := range msg.Parts {
		data := p.Data
		if data == "" {
			data = "{}"
		}
		var toolCall, sig, pforeign any
		if p.ToolCallID != "" {
			toolCall = p.ToolCallID
		}
		if p.Signature != "" {
			sig = p.Signature
		}
		if p.ForeignID != "" {
			pforeign = p.ForeignID
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO part (id, message_id, session_id, seq, type, tool_call_id, signature, data, foreign_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.NewID(), msgID, sessionID, i, p.Type, toolCall, sig, data, pforeign); err != nil {
			return 0, fmt.Errorf("store: ingest insert part: %w", err)
		}
	}
	return len(msg.Parts), nil
}

// sessionTitleFallback names an untitled thread by the first 80 chars of
// its first user text part. Whitespace is collapsed so a multiline prompt
// lists as one line; overlong titles cut on a rune boundary.
func sessionTitleFallback(msgs []Message) string {
	for _, m := range msgs {
		if m.Role != RoleUser {
			continue
		}
		for _, p := range m.Parts {
			if p.Type != "text" {
				continue
			}
			text := partText(p.Data)
			text = strings.Join(strings.Fields(text), " ")
			if text == "" {
				continue
			}
			if utf8.RuneCountInString(text) > 80 {
				runes := []rune(text)
				return string(runes[:80])
			}
			return text
		}
	}
	return ""
}

// partText pulls {"text":...} out of a text part's data. Anything
// unparseable is no title rather than an error: the fallback names a row,
// it never fails an import.
func partText(data string) string {
	var v struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		return ""
	}
	return v.Text
}
