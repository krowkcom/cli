// Command krowk-mock runs a local stand-in for api.krowk.com, so the CLI can be
// developed and demoed without Postgres, object storage or a Rails process. It
// stands in for object storage too, on a /_storage path, so the links it hands
// out actually serve the bytes.
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
	fmt.Printf("stand-in krowk registry on %s\n", base)
	fmt.Printf("  KROWK_API_URL=%s/v1 krowk push <file>\n", base)

	if err := http.ListenAndServe(*addr, registry.Handler(*limit, *site)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
