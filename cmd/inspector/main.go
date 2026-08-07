// Command inspector runs a project's own check command against one exact
// commit and reports green or red. See SPEC.md and README.md.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/inactdev/inspector/internal/inspector"
)

// Exit codes reserve 0, 1, and 2 for verdicts only: 0 green, 1 red, 2
// refused (inspector tried to reach a verdict and could not - no check
// command configured, a dirty working tree, a --commit mismatch, or an
// infrastructure failure). exitUsage is for everything that is not a
// verdict attempt at all - --help, an unrecognized flag, bad usage - so
// a caller can never mistake a help request for a result. 64 follows the
// BSD sysexits.h convention for a command-line usage error (EX_USAGE).
const (
	exitGreen   = 0
	exitRed     = 1
	exitRefused = 2
	exitUsage   = 64
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("inspector", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repoPath := fs.String("repo", ".", "path to the repository to inspect")
	expectCommit := fs.String("commit", "", "assert HEAD resolves to this commit (full or short SHA, branch, tag - anything git resolves unambiguously); refuse on mismatch or ambiguity")
	fs.Usage = func() {
		fmt.Fprintf(stderr, `usage: inspector [flags] [claim text...]

Runs the project's own check command against the repo's current HEAD
commit and reports green or red. See README.md for setup.

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
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
