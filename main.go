// Command theory-server hosts a team's vaults and holds the shared compile
// lock, marker and reports. See internal/theory.
package main

import (
	"os"

	"github.com/imtiazkun/theory-server/internal/theory"
)

func main() {
	os.Exit(theory.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
