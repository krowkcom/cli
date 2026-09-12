package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/huh"

	"github.com/krowkcom/cli/internal/api"
	"github.com/krowkcom/cli/internal/output"
	"github.com/krowkcom/cli/internal/runctx"
	"github.com/krowkcom/cli/internal/store"
)

// interactive reports whether a command may put a question on the terminal
// instead of failing for want of an argument.
//
// The bar is deliberately high, because the callers this CLI is built for
// cannot answer one: a prompt shown to an agent, a pipe or a CI job is not a
// question, it is a hang. So a picker only ever appears where a person is
// demonstrably present — stdout is a terminal and the output is the human
// kind. Asking for JSON is itself the tell: no one asks for machine output
// and then expects to be talked to.
func interactive(f flags, format output.Format, env runctx.Env, isTTY bool) bool {
	return isTTY && format == output.Human && !f.quiet && !inCI(env)
}

// pickWorkspace asks which stored key was meant, and answers with the
// workspace name picked. The options are the store's contents — this picker
// can only ever select among keys already on the machine, which is what makes
// it safe to offer without confirming anything else.
func pickWorkspace(title string, stored []api.WorkspaceKey) (string, error) {
	options := make([]huh.Option[string], 0, len(stored))
	for _, k := range stored {
		// The title leads and the slug follows: the title is what a person
		// recognises, the slug is what the choice actually is — it is the value
		// selected, stored and matched, because titles can be renamed under a
		// key and slugs cannot. A key stored before the registry sent titles
		// has only its slug to show.
		label := k.Name
		if k.WorkspaceName != "" {
			label = k.WorkspaceName + "  —  " + k.Name
		}
		if k.Default {
			label += "  (default)"
		}
		options = append(options, huh.NewOption(label, k.Name))
	}

	var choice string
	err := huh.NewSelect[string]().
		Title(title).
		Options(options...).
		Value(&choice).
		Run()
	if err != nil {
		// Esc or ctrl-c is a person saying "never mind", which is not a failure
		// of anything — but it must not read as a selection either, so the
		// command it interrupts reports it and does nothing.
		return "", api.Fail("selection_cancelled", "nothing was selected and nothing was changed")
	}
	return choice, nil
}

// pickSession asks which session was meant, answering with its store id.
// The options are the listed rows — this picker can only ever select among
// sessions already in the store, which is what makes it safe to offer.
func pickSession(rows []store.SessionRow) (string, error) {
	options := make([]huh.Option[string], 0, len(rows))
	for _, r := range rows {
		title := cleanCell(r.Title)
		if title == "" {
			title = "(untitled)"
		}
		// Cap the title: picker labels are terminal rows built from
		// caller-controlled transcript text, so overlong titles are cut
		// on a rune boundary like the human table.
		if r := []rune(title); len(r) > 60 {
			title = string(r[:57]) + "..."
		}
		// Harness and model join without strays when one is missing; the
		// recency plus short id tell apart the duplicate titles every
		// agent eventually produces ("session title" × 40).
		hm := cleanCell(strings.TrimSpace(strings.TrimSpace(r.Harness) + " " + strings.TrimSpace(r.Model)))
		short := r.ID
		if rs := []rune(short); len(rs) > 8 {
			short = string(rs[:8])
		}
		label := title
		if hm != "" {
			label += "  —  " + hm
		}
		label += fmt.Sprintf("  ·  %s  ·  %s", relativeTime(r.TimeUpdated, time.Now()), short)
		options = append(options, huh.NewOption(label, r.ID))
	}

	var choice string
	err := huh.NewSelect[string]().
		Title("Pick a session").
		Options(options...).
		Value(&choice).
		Run()
	if err != nil {
		return "", api.Fail("selection_cancelled", "nothing was selected and nothing was changed")
	}
	return choice, nil
}
