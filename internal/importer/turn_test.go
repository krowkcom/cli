package importer

import (
	"testing"

	"github.com/krowkcom/cli/internal/store"
)

// Acceptance: hook attachment lines and tool_result-only user lines do not
// open a new turn; a real user prompt does.
func TestStartsTurn(t *testing.T) {
	tests := []struct {
		name string
		cand TurnCandidate
		want bool
	}{
		{
			name: "a real user prompt opens a turn",
			cand: TurnCandidate{Role: store.RoleUser, PartTypes: []string{PartText}},
			want: true,
		},
		{
			name: "a prompt with an image attached is still a prompt",
			cand: TurnCandidate{Role: store.RoleUser, PartTypes: []string{PartImage, PartText}},
			want: true,
		},
		{
			name: "a hook-injected line is not",
			cand: TurnCandidate{Role: store.RoleUser, Meta: true, PartTypes: []string{PartText}},
			want: false,
		},
		{
			name: "an attachment line is not",
			cand: TurnCandidate{Role: store.RoleUser, Attachment: true, PartTypes: []string{PartFile}},
			want: false,
		},
		{
			name: "a tool_result-only user line is the protocol talking",
			cand: TurnCandidate{Role: store.RoleUser, PartTypes: []string{PartToolResult, PartToolResult}},
			want: false,
		},
		{
			name: "a tool_result carrying prose alongside it is a prompt",
			cand: TurnCandidate{Role: store.RoleUser, PartTypes: []string{PartToolResult, PartText}},
			want: true,
		},
		{
			name: "an interrupt ends work rather than starting it",
			cand: TurnCandidate{Role: store.RoleUser, Interrupt: true, PartTypes: []string{PartText}},
			want: false,
		},
		{
			name: "a user line with no content at all is an artefact",
			cand: TurnCandidate{Role: store.RoleUser},
			want: false,
		},
		{
			name: "an assistant message never opens a turn",
			cand: TurnCandidate{Role: store.RoleAssistant, PartTypes: []string{PartText}},
			want: false,
		},
		{
			name: "a tool message never opens a turn",
			cand: TurnCandidate{Role: store.RoleTool, PartTypes: []string{PartToolResult}},
			want: false,
		},
		{
			name: "a system message never opens a turn",
			cand: TurnCandidate{Role: store.RoleSystem, PartTypes: []string{PartText}},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StartsTurn(tt.cand); got != tt.want {
				t.Fatalf("StartsTurn = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSplitTurns(t *testing.T) {
	prompt := TurnCandidate{Role: store.RoleUser, PartTypes: []string{PartText}}
	reply := TurnCandidate{Role: store.RoleAssistant, PartTypes: []string{PartText}}
	toolCall := TurnCandidate{Role: store.RoleAssistant, PartTypes: []string{PartToolCall}}
	toolResult := TurnCandidate{Role: store.RoleUser, PartTypes: []string{PartToolResult}}
	hook := TurnCandidate{Role: store.RoleUser, Meta: true, PartTypes: []string{PartText}}
	attachment := TurnCandidate{Role: store.RoleUser, Attachment: true, PartTypes: []string{PartFile}}

	tests := []struct {
		name string
		in   []TurnCandidate
		want []TurnSpan
	}{
		{name: "nothing at all", in: nil, want: nil},
		{
			name: "one prompt and its whole tool loop is one turn",
			in:   []TurnCandidate{prompt, toolCall, toolResult, reply},
			want: []TurnSpan{{0, 4}},
		},
		{
			name: "two prompts are two turns",
			in:   []TurnCandidate{prompt, reply, prompt, reply},
			want: []TurnSpan{{0, 2}, {2, 4}},
		},
		{
			name: "hooks and attachments stay inside the turn they interrupt",
			in:   []TurnCandidate{prompt, hook, attachment, toolCall, toolResult, reply},
			want: []TurnSpan{{0, 6}},
		},
		{
			name: "a preamble before the first prompt gets its own span",
			in:   []TurnCandidate{hook, prompt, reply},
			want: []TurnSpan{{0, 1}, {1, 3}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SplitTurns(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("SplitTurns = %+v, want %+v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("SplitTurns = %+v, want %+v", got, tt.want)
				}
			}
			// Whatever the shape, every message belongs to exactly one span.
			covered := 0
			for _, s := range got {
				covered += s.End - s.Start
			}
			if covered != len(tt.in) {
				t.Fatalf("spans cover %d of %d messages", covered, len(tt.in))
			}
		})
	}
}
