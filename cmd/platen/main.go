// Command platen is a print management server with distributed print workers.
package main

import (
	"fmt"
	"os"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

const usage = `platen - print management server with distributed print workers

Usage:
  platen server  -config <file>   run the management server
  platen worker  -config <file>   run a standalone worker
  platen admin   <command> ...    administer a server over NetBird
  platen version                  print the version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version":
		fmt.Println(version)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}
