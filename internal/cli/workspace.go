package cli

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/krowkcom/cli/internal/api"
	"github.com/krowkcom/cli/internal/config"
	"github.com/krowkcom/cli/internal/output"
	"github.com/krowkcom/cli/internal/runctx"
)

// workspacesList answers both workspace questions at once: which keys this
// machine holds, and which of them a command run right here would use. The
// second half is the one that changes with the directory, which is why it is
// computed rather than read back from anywhere.
func workspacesList(w io.Writer, f flags, format output.Format, env runctx.Env, colour bool) error {
	ws, source, err := resolveWorkspace(f, env)
	if err != nil {
		return err
	}

	stored := api.StoredWorkspaces()
	view := output.Workspaces{Stored: stored, Resolved: ws, Source: source,
		Shadowed: env("KROWK_TOKEN") != ""}
	// Nothing asked by name means the stored default answers, and the listing
	// should say so rather than showing a default mark the reader has to
	// cross-reference against an empty "resolved" line.
	if ws == "" {
		for _, k := range stored {
			if k.Default {
				view.Resolved, view.Source = k.Name, "stored default"
			}
		}
	}
	fmt.Fprintln(w, output.WorkspaceList(view, format, f.quiet, colour))
	return nil
}

// workspacesUse repoints the machine-wide default at another stored key. It is
// selection, not authentication: it can only name a key `auth login` already
// stored, which is what makes it safe to run without confirming anything.
//
// With no argument and a person at the terminal, the store's contents become
// a picker — the answer was always going to be one of those names, so asking
// beats failing and having them run the listing to copy one out. Everywhere
// else, no argument stays the error it always was.
func workspacesUse(w io.Writer, args []string, f flags, format output.Format, env runctx.Env,
	colour, isTTY bool) error {
	if len(args) == 0 {
		name, err := askForWorkspace("Which workspace should be the default?", f, format, env, isTTY,
			api.Fail("no_workspace",
				"pass the workspace: `krowk workspaces use <name>` — `krowk workspaces` lists them"))
		if err != nil {
			return err
		}
		args = []string{name}
	}
	path, err := api.SetDefaultWorkspace(args[0])
	if err != nil {
		// The store's own message already lists what is there or says the store
		// is empty, so it is surfaced as written rather than paraphrased.
		return api.Fail("unknown_workspace", err.Error())
	}
	fmt.Fprintln(w, output.DefaultWorkspace(args[0], path, format, f.quiet, colour))
	return nil
}

// configShow reports the effective configuration and where each value came
// from. A malformed file still fails it, even though a broken config is
// exactly what someone running this is often chasing: Load cannot say what the
// unreadable file would have set, so the honest answer is the error naming the
// file — which is the diagnosis, delivered as a failure.
func configShow(w io.Writer, f flags, format output.Format, env runctx.Env, colour bool) error {
	cfg, err := config.Load("", env, config.Overrides{Workspace: f.workspace})
	if err != nil {
		return api.Fail("bad_config", err.Error())
	}

	view := output.ConfigView{
		Workspace:  cfg.Workspace,
		Sources:    cfg.Sources,
		GlobalPath: config.GlobalPath(),
	}
	if repo, inRepo := config.RepoPath(""); inRepo {
		view.RepoPath = repo
	}
	fmt.Fprintln(w, output.ConfigShow(view, format, f.quiet, colour))
	return nil
}

// configSet writes one key into a config file: the repository's by default,
// because pinning the repo is what the command exists for, or the global file
// behind --global. Outside a git repository there is no repo file to write, and
// guessing a directory would pin something other than what was meant.
//
// `config set workspace` with the value left off asks, when there is a person
// to ask — the value is one of the stored names, and the picker offers exactly
// those. Other keys have no store to offer from, so their missing value stays
// an error everywhere.
func configSet(w io.Writer, args []string, f flags, format output.Format, env runctx.Env,
	colour, isTTY bool) error {
	missing := api.Fail("missing_argument",
		"pass the key and the value: `krowk config set workspace <name>` — "+
			"keys: "+strings.Join(config.KnownKeys(), ", "))
	if len(args) == 0 {
		return missing
	}
	if len(args) == 1 {
		if args[0] != "workspace" {
			return missing
		}
		name, err := askForWorkspace("Which workspace should this pin?", f, format, env, isTTY, missing)
		if err != nil {
			return err
		}
		args = append(args, name)
	}
	key, value := args[0], args[1]
	if err := knownConfigKey(key); err != nil {
		return err
	}

	path, err := configFile(f)
	if err != nil {
		return err
	}
	if err := config.Set(path, key, value); err != nil {
		return api.Fail("config_unwritable", "could not write "+path+": "+err.Error())
	}
	fmt.Fprintln(w, output.ConfigWrote(key, value, path, format, f.quiet, colour))
	return nil
}

// configUnset removes one key from a config file, same file selection as
// configSet. Removing a key that was never set is success — the state asked
// for is the state there is.
func configUnset(w io.Writer, args []string, f flags, format output.Format, colour bool) error {
	if len(args) == 0 {
		return api.Fail("missing_argument",
			"pass the key: `krowk config unset workspace` — keys: "+strings.Join(config.KnownKeys(), ", "))
	}
	if err := knownConfigKey(args[0]); err != nil {
		return err
	}

	path, err := configFile(f)
	if err != nil {
		return err
	}
	if err := config.Unset(path, args[0]); err != nil {
		return api.Fail("config_unwritable", "could not write "+path+": "+err.Error())
	}
	fmt.Fprintln(w, output.ConfigWrote(args[0], "", path, format, f.quiet, colour))
	return nil
}

// configFile is which file `config set` and `config unset` write: the
// repository's, or the global one when --global asks for it.
func configFile(f flags) (string, error) {
	if f.global {
		return config.GlobalPath(), nil
	}
	repo, inRepo := config.RepoPath("")
	if !inRepo {
		return "", api.Fail("not_in_a_repository",
			"the repo config lives at <git-root>/.krowk/config.json and there is no git root here — "+
				"run it inside the repository, or pass --global for the machine-wide file")
	}
	return repo, nil
}

// askForWorkspace turns a missing argument into a question when a person is
// there to answer it; anywhere else it answers with `otherwise`, the error
// the command would have raised anyway. The store's emptiness is checked
// before the terminal is touched: a picker over nothing is a question with no
// answers, and the fix for it is a login, not a selection.
func askForWorkspace(title string, f flags, format output.Format, env runctx.Env,
	isTTY bool, otherwise error) (string, error) {
	if !interactive(f, format, env, isTTY) {
		return "", otherwise
	}
	stored := api.StoredWorkspaces()
	if len(stored) == 0 {
		return "", api.Fail("not_authenticated",
			"no keys are stored to pick from — `krowk auth login` adds one")
	}
	return pickWorkspace(title, stored)
}

// knownConfigKey rejects a key this build has no idea of, before any file is
// touched. Writing it anyway would succeed and then do nothing, which reads as
// krowk ignoring the user — worse than saying no.
func knownConfigKey(key string) error {
	if slices.Contains(config.KnownKeys(), key) {
		return nil
	}
	return api.Fail("unknown_config_key",
		"`"+key+"` is not a configuration key — keys: "+strings.Join(config.KnownKeys(), ", "))
}
