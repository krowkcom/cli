package cli

import (
	"github.com/charmbracelet/huh"

	"github.com/krowkcom/cli/internal/api"
	"github.com/krowkcom/cli/internal/output"
	"github.com/krowkcom/cli/internal/runctx"
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
		label := k.Name
		if k.KeyID != "" {
			label += "  ·  " + k.KeyID
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
