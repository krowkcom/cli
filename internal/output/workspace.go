package output

import (
	"fmt"
	"strings"

	"github.com/krowkcom/cli/internal/api"
)

// Workspaces is what `krowk workspaces` reports: every key the machine holds,
// and which workspace a command run here would actually use. The two halves
// answer different questions — "what could I use" is the store, "what will be
// used" is the resolution — and an agent deciding whether to log in needs both.
type Workspaces struct {
	Stored []api.WorkspaceKey `json:"stored"`
	// Resolved is the workspace uploads from this directory land in, and Source
	// says which layer decided it — a flag, the environment, a config file, or
	// the store's own default. Empty when nothing resolves, which is the
	// anonymous case.
	Resolved string `json:"resolved,omitempty"`
	Source   string `json:"source,omitempty"`
	// Shadowed says KROWK_TOKEN is set, which outranks every stored key: the
	// resolution below names the key krowk would have picked, and the
	// environment is using a different one. Without this line the listing
	// would answer the question wrongly with confidence.
	Shadowed bool `json:"shadowed_by_env,omitempty"`
	// KeyMissing says the resolved workspace cannot actually produce a key, so
	// every upload here fails — the one qualification without which "uploads
	// from here land in X" would be a lie told to whoever reads the listing.
	KeyMissing bool `json:"key_missing,omitempty"`
}

// WorkspaceList renders the store. There is no link to a workspace, so
// markdown and url fall back to the JSON envelope.
func WorkspaceList(ws Workspaces, f Format, quiet, colour bool) string {
	if f != Human {
		if quiet {
			return encode(ws)
		}
		return encode(Envelope{
			OK:          true,
			Data:        ws,
			Summary:     workspacesSummary(ws),
			Breadcrumbs: workspacesCrumbs(ws),
		})
	}

	if len(ws.Stored) == 0 {
		return "no keys stored — `krowk auth login` adds one; until then uploads are anonymous and expire"
	}

	lines := []string{"stored keys"}
	for _, k := range ws.Stored {
		mark := ""
		if k.Default {
			mark = "  " + paint(colour, dim, "(default)")
		}
		// The title leads where login recorded one; the slug is always there,
		// because it is the value every command and config file speaks in.
		name := k.Name
		if k.WorkspaceName != "" {
			name = k.WorkspaceName + " — " + k.Name
		}
		lines = append(lines, fmt.Sprintf("  %-40s %s%s", name, k.KeyID, mark))
	}
	switch {
	case ws.Resolved != "" && ws.KeyMissing:
		lines = append(lines, fmt.Sprintf("%s resolves here (%s) — but no key is stored for it, "+
			"so every upload fails until `krowk auth login`", ws.Resolved, ws.Source))
	case ws.Resolved != "":
		lines = append(lines, fmt.Sprintf("uploads from here land in %s — %s", ws.Resolved, ws.Source))
	}
	if ws.Shadowed {
		lines = append(lines, paint(colour, dim,
			"! KROWK_TOKEN is set and wins over every stored key — uploads use that key instead"))
	}
	return strings.Join(lines, "\n")
}

// workspacesSummary is the one line an agent reads first, and it leads with the
// resolution because that is the actionable half: the store's contents only
// matter when what resolves is not what was wanted.
func workspacesSummary(ws Workspaces) string {
	if ws.Shadowed {
		return "KROWK_TOKEN is set and wins over every stored key — `krowk auth verify` names " +
			"the workspace that key acts in"
	}
	stored := fmt.Sprintf("%d key(s) stored", len(ws.Stored))
	if ws.Resolved == "" {
		if len(ws.Stored) == 0 {
			return "no keys stored — uploads are anonymous"
		}
		return stored + ", nothing resolves here — uploads are anonymous"
	}
	if ws.KeyMissing {
		return fmt.Sprintf("%s resolves here (%s) but holds no key — every upload fails "+
			"until `krowk auth login`", ws.Resolved, ws.Source)
	}
	return fmt.Sprintf("uploads from here land in %s (%s) — %s", ws.Resolved, ws.Source, stored)
}

// workspacesCrumbs points at the two ways to change the answer: the store's
// default, and the repo's say-so. Login is only suggested when the store is
// empty, because with keys in hand the fix for a wrong workspace is selection,
// not another key.
func workspacesCrumbs(ws Workspaces) []Breadcrumb {
	if len(ws.Stored) == 0 {
		return []Breadcrumb{{
			Action: "log in",
			Cmd:    "krowk auth login",
			Description: "approving it in the browser stores a key, and uploads stop " +
				"expiring — `--token krowk_sk_...` stores one directly instead",
		}}
	}
	return []Breadcrumb{
		{
			Action: "switch the default",
			Cmd:    "krowk workspaces use <workspace>",
			Description: "repoints the machine-wide default at another stored key — " +
				"<workspace> is a name from this list",
		},
		{
			Action: "pin this repository",
			Cmd:    "krowk config set workspace <workspace>",
			Description: "writes .krowk/config.json at the repository root, so every command " +
				"run inside it uses that workspace's key whoever runs it — <workspace> is a " +
				"name from this list",
		},
	}
}

// DefaultWorkspace is the receipt for `workspaces use`: which stored key is
// now the machine-wide fallback, and where that pointer was written. It is
// only the fallback — a repo config or the environment still outranks it, and
// the crumb points at the command that says what actually resolves.
func DefaultWorkspace(name, path string, f Format, quiet, colour bool) string {
	type receipt struct {
		Default string `json:"default"`
		Path    string `json:"path"`
	}
	r := receipt{Default: name, Path: path}

	if f != Human {
		if quiet {
			return encode(r)
		}
		return encode(Envelope{
			OK:      true,
			Data:    r,
			Summary: "default workspace is now " + name,
			Breadcrumbs: []Breadcrumb{{
				Action:      "check what resolves here",
				Cmd:         "krowk workspaces",
				Description: "a repo config, KROWK_WORKSPACE or --workspace still outranks the default",
			}},
		})
	}
	return fmt.Sprintf("%s default workspace is now %s", paint(colour, green, "✓"), name)
}

// ConfigView is the effective configuration as `krowk config show` reports it:
// each value alongside which layer said so, and where the two files live —
// including the one that does not exist yet, because that is where `config set`
// would write.
type ConfigView struct {
	Workspace string `json:"workspace,omitempty"`
	// Sources says which layer each value came from, keyed by config key.
	Sources    map[string]string `json:"sources,omitempty"`
	GlobalPath string            `json:"global_path"`
	// RepoPath is empty outside a git repository, where there is no repo layer
	// to write to.
	RepoPath string `json:"repo_path,omitempty"`
}

// ConfigShow renders the effective configuration.
func ConfigShow(v ConfigView, f Format, quiet, colour bool) string {
	if f != Human {
		if quiet {
			return encode(v)
		}
		summary := "no configuration set — uploads use the stored default key"
		if v.Workspace != "" {
			summary = fmt.Sprintf("workspace %s — %s", v.Workspace, v.Sources["workspace"])
		}
		return encode(Envelope{
			OK:      true,
			Data:    v,
			Summary: summary,
			Breadcrumbs: []Breadcrumb{{
				Action: "set the workspace",
				Cmd:    "krowk config set workspace <workspace>",
				Description: "pins which workspace commands in this repository use — " +
					"--global sets the machine-wide fallback instead",
			}},
		})
	}

	var lines []string
	if v.Workspace != "" {
		lines = append(lines, fmt.Sprintf("%-11s %s  %s", "workspace", v.Workspace,
			paint(colour, dim, "("+v.Sources["workspace"]+")")))
	} else {
		lines = append(lines, paint(colour, dim,
			"nothing set — uploads use the stored default key"))
	}
	lines = append(lines, fmt.Sprintf("  %-9s %s", "global", v.GlobalPath))
	if v.RepoPath != "" {
		lines = append(lines, fmt.Sprintf("  %-9s %s", "repo", v.RepoPath))
	}
	return strings.Join(lines, "\n")
}

// ConfigWrote is the receipt for `config set` and `config unset`: which key, in
// which file, and what it now says. value is empty for an unset.
func ConfigWrote(key, value, path string, f Format, quiet, colour bool) string {
	type receipt struct {
		Key   string `json:"key"`
		Value string `json:"value,omitempty"`
		Path  string `json:"path"`
	}
	r := receipt{Key: key, Value: value, Path: path}

	if f != Human {
		if quiet {
			return encode(r)
		}
		summary := fmt.Sprintf("%s = %s in %s", key, value, path)
		if value == "" {
			summary = fmt.Sprintf("%s removed from %s", key, path)
		}
		return encode(Envelope{
			OK:      true,
			Data:    r,
			Summary: summary,
			Breadcrumbs: []Breadcrumb{{
				Action:      "check the effective configuration",
				Cmd:         "krowk config show",
				Description: "reports what actually resolves here, which layer wins, and why",
			}},
		})
	}

	tick := paint(colour, green, "✓")
	if value == "" {
		return fmt.Sprintf("%s %s removed from %s", tick, key, path)
	}
	return fmt.Sprintf("%s %s = %s in %s", tick, key, value, path)
}
