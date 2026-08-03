// Command krowk-mock runs a local stand-in for api.krowk.com, so the CLI can be
// developed and demoed before the real registry exists.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"

	"github.com/krowkcom/cli/internal/registry"
)

func main() {
	addr := flag.String("addr", ":8787", "listen address")
	limit := flag.Int64("limit-bytes", registry.DefaultLimitBytes, "reject uploads above this size")
	site := flag.String("site", "", "origin for returned links (default: the request host)")
	flag.Parse()

	base := "http://localhost" + *addr
	fmt.Printf("mock krowk registry on %s\n", base)
	fmt.Printf("  KROWK_API_URL=%s/v1 krowk uploads create <file>\n", base)

	if err := http.ListenAndServe(*addr, registry.Handler(*limit, *site)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
