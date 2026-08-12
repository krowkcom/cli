// Command krowk-mcp exposes krowk to agents over MCP, as a thin client over the
// same /v1 API the CLI uses.
//
// It speaks MCP over stdio, so stdout carries protocol traffic and nothing else
// — every diagnostic goes to stderr.
package main

import (
	"context"
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
	root := flag.String("root", "",
		"only upload files under this directory (default: the working directory)")
	flag.Parse()

	if *version {
		fmt.Println(cli.Version)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// The same workspace resolution as the CLI, read once at startup from
	// where the server was launched — an MCP server lives inside one editor
	// session in one checkout, so the repo config that governs `krowk push`
	// there governs its uploads too. A malformed config file is fatal for the
	// same reason it fails the CLI: serving on while ignoring a file written
	// to steer uploads would land them exactly where it says not to.
	cfg, err := config.Load("", os.Getenv, config.Overrides{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "krowk-mcp:", err)
		os.Exit(1)
	}
	token := api.ReadToken(os.Getenv, cfg.Workspace)
	if token == "" && cfg.Workspace != "" {
		fmt.Fprintln(os.Stderr, "krowk-mcp: no key is stored for workspace "+cfg.Workspace+
			" — run `krowk auth login`, or `krowk workspaces` to see what is stored")
		os.Exit(1)
	}

	server := &mcp.Server{
		// Same registry precedence as the CLI, so KROWK_DEV points both at a
		// local registry without either needing its own plumbing.
		Client: api.New(api.BaseURLFor(false, os.Getenv), token),
		Env:    runctx.Env(os.Getenv),
		// Uploads are confined here. An artifact needs no credential to read, and
		// the model picks the paths, so the boundary is what keeps an instruction
		// hidden in a repository file from publishing something else entirely.
		Root:    firstNonEmpty(*root, os.Getenv("KROWK_MCP_ROOT")),
		Version: cli.Version,
	}

	if err := server.Serve(ctx, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "krowk-mcp:", err)
		os.Exit(1)
	}
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
