// Command inspector runs a project's own check command against one exact
// commit and reports green or red. See SPEC.md and README.md.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/inactdev/inspector/internal/inspector"
)

// Exit codes are a deliberate three-way split: 0 and 1 are a verdict on
// the code, 2 means inspector did not reach a verdict at all (refused or
// hit an infrastructure error). Callers must not treat 2 as red.
const (
	exitGreen   = 0
	exitRed     = 1
	exitRefused = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("inspector", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repoPath := fs.String("repo", ".", "path to the repository to inspect")
	expectCommit := fs.String("commit", "", "assert HEAD equals this commit; refuse on mismatch")
	fs.Usage = func() {
		fmt.Fprintf(stderr, `usage: inspector [flags] [claim text...]

Runs the project's own check command against the repo's current HEAD
commit and reports green or red. See README.md for setup.

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitRefused
	}

	opts := inspector.Options{
		RepoPath:     *repoPath,
		Claim:        strings.Join(fs.Args(), " "),
		ExpectCommit: *expectCommit,
		Stdout:       stdout,
		Stderr:       stderr,
	}

	result, err := inspector.Run(opts)
	if err != nil {
		fmt.Fprintf(stderr, "inspector: %v\n", err)
		return exitRefused
	}

	switch result.Outcome {
	case inspector.Green:
		fmt.Fprintf(stdout, "\ngreen - %s (report: %s)\n", result.Commit, result.ReportPath)
		return exitGreen
	case inspector.Red:
		fmt.Fprintf(stdout, "\nred - %s (report: %s)\n", result.Commit, result.ReportPath)
		return exitRed
	default: // inspector.Refused
		fmt.Fprintf(stderr, "refused: %s\n", result.Message)
		return exitRefused
	}
}
