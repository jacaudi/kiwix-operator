// Command kiwix-operator is the single binary behind the manager, librarian and upload roles.
package main

import (
	"fmt"
	"io"
	"os"
)

// Stamped at build time via -ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

type subcommand func(args []string, stdout, stderr io.Writer) int

// subcommands maps each role to its entry point; the not-yet-wired stubs are replaced as each role lands.
var subcommands = map[string]subcommand{
	"manager":   notWired("manager"),
	"librarian": notWired("librarian"),
	"upload":    notWired("upload"),
}

func notWired(name string) subcommand {
	return func(_ []string, _, stderr io.Writer) int {
		_, _ = fmt.Fprintf(stderr, "%s: not yet wired\n", name)
		return 3
	}
}

func versionString() string {
	return fmt.Sprintf("kiwix-operator %s (commit %s, built %s)", version, commit, date)
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "--version" {
		_, _ = fmt.Fprintln(stdout, versionString())
		return 0
	}
	if len(args) > 0 {
		if sub, ok := subcommands[args[0]]; ok {
			return sub(args[1:], stdout, stderr)
		}
	}
	_, _ = fmt.Fprintln(stderr, "usage: kiwix-operator <manager|librarian|upload>")
	return 2
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
