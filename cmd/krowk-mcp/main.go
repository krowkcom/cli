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
	"github.com/krowkcom/cli/internal/mcp"
	"github.com/krowkcom/cli/internal/runctx"
)

func main() {
	version := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *version {
		fmt.Println(cli.Version)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	server := &mcp.Server{
		Client:  api.New(os.Getenv("KROWK_API_URL"), api.ReadToken(os.Getenv)),
		Env:     runctx.Env(os.Getenv),
		Version: cli.Version,
	}

	if err := server.Serve(ctx, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "krowk-mcp:", err)
		os.Exit(1)
	}
}
