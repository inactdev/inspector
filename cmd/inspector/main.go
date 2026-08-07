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
// refused (inspector tried to reach a verdict and could not - see
// inspector.Refused for those causes, plus any infrastructure failure
// Run reports as an error). exitUsage is for everything that is not a
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
		RepoPath: *repoPath,
		Claim:    strings.Join(fs.Args(), " "),
		Stdout:   stdout,
		Stderr:   stderr,
	}

	result, err := inspector.Run(opts)
	if err != nil {
		fmt.Fprintf(stderr, "inspector: %v\n", err)
		return exitRefused
	}

	if result.Warning != "" {
		fmt.Fprintf(stderr, "\n%s\n%s\n%s\n\n", warningBar, result.Warning, warningBar)
	}

	switch result.Outcome {
	case inspector.Green:
		fmt.Fprintf(stdout, "\ngreen - %s%s\n", result.Commit, reportSuffix(result.ReportPath))
		return exitGreen
	case inspector.Red:
		fmt.Fprintf(stdout, "\nred - %s%s\n", result.Commit, reportSuffix(result.ReportPath))
		return exitRed
	default: // inspector.Refused
		fmt.Fprintf(stderr, "refused: %s%s\n", result.Message, reportSuffix(result.ReportPath))
		return exitRefused
	}
}

// warningBar makes a Warning impossible to miss among a check command's
// own output, which inspector streams live right above it.
const warningBar = "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!"

// reportSuffix formats the report-path annotation, or nothing when
// there is no report - a real possibility now that a save failure
// (Warning) or a refusal that never ran a check both leave it empty.
func reportSuffix(path string) string {
	if path == "" {
		return ""
	}
	return fmt.Sprintf(" (report: %s)", path)
}
