package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/krowkcom/cli/internal/api"
	"github.com/krowkcom/cli/internal/config"
	"github.com/krowkcom/cli/internal/output"
)

// showHelp answers "what can you do", for a person or for a program.
//
// topic is what was asked about, already stripped of the word `help`: empty for
// the whole surface, or the words naming one command. `krowk help uploads
// attach` and `krowk uploads attach --help` arrive here identically, because
// they are the same question.
//
// JSON is the machine surface and it is complete: the same catalog the human
// text is rendered from, with nothing summarised away. Which format is used
// follows the CLI's standing rule rather than a special case for help — human
// on a terminal, JSON when the output is piped or asked for — so a script that
// captures `krowk help` gets something it can parse for the same reason
// `krowk push` does. markdown and url are the two paste forms of an artifact's
// link — an embed and the card page's URL — and have nothing to say about a
// command, so they fall back to the text a person would read.
func showHelp(w io.Writer, topic []string, format output.Format, f flags) error {
	c := Surface()

	if len(topic) == 0 {
		if format == output.JSON {
			return encodeJSON(w, c, f)
		}
		fmt.Fprintf(w, helpTemplate, Version, usageBlock(c),
			api.DevBaseURL, api.DefaultBaseURL,
			api.CredentialsPath(), config.GlobalPath())
		return nil
	}

	cmd, ok := c.Find(topic)
	if !ok {
		// The same refusal an unrunnable command gets, because it is the same
		// mistake: a name krowk does not have. Asking about it must not print the
		// whole help and exit 0, which would read as though the command existed.
		return api.Fail("unknown_command",
			"`"+strings.Join(clip(topic, 2), " ")+"` is not a krowk command — run `krowk help` for the list")
	}
	if format == output.JSON {
		return encodeJSON(w, cmd, f)
	}
	fmt.Fprintln(w, commandHelp(cmd))
	return nil
}

// encodeJSON writes the surface indented, the way every other JSON answer from
// krowk is written, and with HTML escaping off so a URL in a usage line stays
// readable. It goes out through emit like any other result, so the surface is
// filterable too: `krowk help --json --jq '.commands[].name'` is the shortest
// way for an agent to learn what krowk can do.
func encodeJSON(w io.Writer, v any, f flags) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return api.Fail("encode_failed", "the help catalog could not be encoded: "+err.Error())
	}
	return emit(w, strings.TrimRight(buf.String(), "\n"), f)
}
