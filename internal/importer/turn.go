package importer

import "github.com/krowkcom/cli/internal/store"

// TurnCandidate is one transcript line reduced to the four things the turn
// rule cares about. It is provider-neutral on purpose: the rule has to give
// the same answer for a Claude JSONL line, a cursor row and an opencode
// message, and it can only do that if each source translates into this shape
// rather than the rule learning three formats.
type TurnCandidate struct {
	// Role is the message role as the source reported it.
	Role store.Role
	// Meta is the source's own "this was not typed by a person" marker:
	// Claude's isMeta, a hook's injected line, a system reminder. Sources
	// set it from whatever their format calls it.
	Meta bool
	// Attachment is a line that exists to carry a file or a pasted blob
	// into the context — Claude's `type: "attachment"` lines. It wears the
	// user role and is not a prompt.
	Attachment bool
	// Interrupt is a cancelled or interrupted request. It ends work rather
	// than starting it, so it must not open a turn that then swallows the
	// next real prompt.
	Interrupt bool
	// PartTypes is the canonical types of the message's parts, in order.
	// The rule reads it to tell a prompt from a bag of tool results.
	PartTypes []string
}

// StartsTurn reports whether a candidate opens a new turn.
//
// The rule is "a turn starts at a user message and ends before the next
// one", with the whole difficulty in what counts as a user message. The
// transcripts are full of user-role lines a person never typed: hook output,
// attached files, system reminders, and above all tool results, which the
// protocol sends back under the user role because that is where the API puts
// them. Counting those would report ten turns where somebody asked one
// question, and every per-turn cost figure downstream would be wrong by the
// same factor.
//
// So a turn opens on a user-role line that is not marked meta, is not an
// attachment, is not an interrupt, and carries at least one part that is not
// a tool result. The last clause is the one that does the real work: a
// tool_result-only user line is the protocol talking, not a person.
func StartsTurn(c TurnCandidate) bool {
	if c.Role != store.RoleUser {
		return false
	}
	if c.Meta || c.Attachment || c.Interrupt {
		return false
	}
	return hasPromptPart(c.PartTypes)
}

// hasPromptPart reports whether any part is something a person could have
// sent. An empty part list is not a prompt: a user line with no content is
// an artefact, not a question.
func hasPromptPart(types []string) bool {
	for _, t := range types {
		if t != PartToolResult {
			return true
		}
	}
	return false
}

// TurnSpan is the half-open range of message indexes belonging to one turn:
// messages Start through End-1.
type TurnSpan struct {
	Start int
	End   int
}

// SplitTurns groups messages into turns by StartsTurn, covering every
// message exactly once.
//
// Lines before the first prompt get their own leading span. Transcripts do
// begin that way — a resumed session, a system preamble, a hook that ran
// before anybody typed — and the alternatives are both worse: dropping them
// loses transcript, and folding them into the first real turn attributes
// somebody else's work to that prompt. A caller that wants them merged can
// see the leading span for what it is, because it does not start at a
// prompt.
//
// The spans are positional, which is what the store wants: turn i is seq i,
// so a re-read of a growing transcript extends the list instead of
// renumbering it — as long as the source re-reads from the start of the
// session, which is why turns are cumulative and messages are not.
func SplitTurns(candidates []TurnCandidate) []TurnSpan {
	var spans []TurnSpan
	for i, c := range candidates {
		// Index zero always opens a span, prompt or not: that is the
		// leading span, and without it the messages before the first
		// prompt would belong to nothing.
		if i != 0 && !StartsTurn(c) {
			continue
		}
		if n := len(spans); n > 0 {
			spans[n-1].End = i
		}
		spans = append(spans, TurnSpan{Start: i, End: len(candidates)})
	}
	return spans
}
