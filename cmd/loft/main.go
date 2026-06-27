// Command loft is the self-serve deploy CLI: upload a folder to loftd over HTTP and serve it as a
// private site. Authenticates via the OAuth device flow (loft login) against any OIDC provider.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/larsakerlund/loft/internal/cli"
)

func main() {
	err := cli.Run(context.Background(), os.Args[1:])
	switch {
	case err == nil:
		return
	case errors.Is(err, cli.ErrUsage):
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
