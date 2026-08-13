// Command krowk-mcp exposes krowk to agents over MCP, as a thin client over the
// same /v1 API the CLI uses.
//
// It speaks MCP over stdio, so stdout carries protocol traffic and nothing else
// — every diagnostic goes to stderr.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/krowkcom/cli/internal/api"
	"github.com/krowkcom/cli/internal/cli"
	"github.com/krowkcom/cli/internal/config"
	"github.com/krowkcom/cli/internal/mcp"
	"github.com/krowkcom/cli/internal/runctx"
)

func main() {
	version := flag.Bool("version", false, "print the version and exit")
	rootFlag := flag.String("root", "",
		"only upload files under this directory (default: the working directory)")
	flag.Parse()

	if *version {
		fmt.Println(cli.Version)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Uploads are confined to the root, and the config is read from the same
	// place — an MCP server lives inside one editor session in one checkout, so
	// the repo config that governs `krowk push` there governs its uploads too.
	//
	// The root rather than the working directory, because they are rarely the
	// same here: an editor spawns the server from / or the home directory and
	// passes the checkout as --root, so walking up from the process's own
	// directory would miss the repository's committed .krowk/config.json
	// entirely, or find an unrelated one belonging to whatever contains the
	// cwd. An empty root already means the working directory, which is what an
	// empty dir means to config.Load, so the two stay in step.
	root := firstNonEmpty(*rootFlag, os.Getenv("KROWK_MCP_ROOT"))

	var token string
	var workspaceErr error
	cfg, err := config.Load(root, os.Getenv, config.Overrides{})
	if err != nil {
		workspaceErr = err
	} else if token, err = api.ResolveToken(os.Getenv, cfg.Workspace); err != nil {
		token, workspaceErr = "", err
	}
	if workspaceErr != nil {
		// Not fatal, on purpose. Half of what this server does needs no
		// credential — reading an artifact back, reporting the run context, an
		// anonymous push and the claim token it comes back with — and exiting
		// here would take all of that away over a key those calls never use.
		// What must not happen is an upload landing in the anonymous workspace
		// when this checkout named its own; the tools that would create
		// something there refuse instead, with this reason attached.
		fmt.Fprintln(os.Stderr, "krowk-mcp:", reason(workspaceErr)+
			" — serving without a key: reads still work, uploads and claims are refused")
	}

	server := &mcp.Server{
		// Same registry precedence as the CLI, so KROWK_DEV points both at a
		// local registry without either needing its own plumbing.
		Client: api.New(api.BaseURLFor(false, os.Getenv), token),
		Env:    runctx.Env(os.Getenv),
		// Uploads are confined here. An artifact needs no credential to read, and
		// the model picks the paths, so the boundary is what keeps an instruction
		// hidden in a repository file from publishing something else entirely.
		Root:         root,
		WorkspaceErr: workspaceErr,
		Version:      cli.Version,
	}

	if err := server.Serve(ctx, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "krowk-mcp:", err)
		os.Exit(1)
	}
}

// reason renders a startup failure for the one stderr line it gets. An
// *api.Error's Error() is only its code, and the code alone does not tell
// anyone what to do, so the fix comes with it.
func reason(err error) string {
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		if fix := apiErr.Fix(); fix != "" {
			return apiErr.Code() + " — " + fix
		}
		return apiErr.Code()
	}
	return err.Error()
}

// firstNonEmpty prefers the flag, then the environment, then the default.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
