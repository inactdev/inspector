// Package inspector runs a project's own check command against one exact
// commit and reports green or red. See SPEC.md for the design this
// implements.
package inspector

import (
	"errors"
	"fmt"
	"io"
	"time"
)

// Outcome is the result of one inspector run.
type Outcome string

const (
	// Green means the project's check command passed.
	Green Outcome = "green"
	// Red means the project's check command failed.
	Red Outcome = "red"
	// Refused means inspector did not run the check at all - no check
	// command configured, a dirty working tree, or a --commit that
	// doesn't resolve to exactly one commit (nonexistent, ambiguous, or
	// resolves but doesn't match HEAD). A refusal is loud on purpose: it
	// must never look like a pass.
	Refused Outcome = "refused"
)

// Options configures one inspector run.
type Options struct {
	// RepoPath is any path inside the git repository to inspect.
	RepoPath string
	// Claim is free-text context for what's being claimed as finished.
	// It is recorded in the local report only - inspector does not act
	// on its content.
	Claim string
	// ExpectCommit, if set, asserts that HEAD must resolve to this
	// commit. Accepts anything git itself would resolve unambiguously -
	// a full SHA, a short prefix, a branch, a tag. A mismatch, or a
	// value that doesn't resolve to exactly one commit, refuses rather
	// than silently inspecting the wrong commit.
	ExpectCommit string

	Stdout io.Writer
	Stderr io.Writer
}

// Result is the outcome of a Run.
type Result struct {
	Outcome    Outcome
	Commit     string
	Message    string // human-readable explanation; always set for Refused
	ReportPath string // set for Green and Red
}

// Run resolves the repo and its HEAD commit, loads the project's check
// command, runs it, and writes a local report. The returned error is
// reserved for infrastructure failures (git missing, disk full); every
// ordinary outcome - including refusals - comes back as a Result.
func Run(opts Options) (Result, error) {
	repoRoot, err := ResolveRepoRoot(opts.RepoPath)
	if err != nil {
		return Result{Outcome: Refused, Message: err.Error()}, nil
	}

	status, err := WorkingTreeStatus(repoRoot)
	if err != nil {
		return Result{}, err
	}
	if status != "" {
		return Result{
			Outcome: Refused,
			Message: "working tree is not clean, so a check run here would not be bound to one exact commit.\n" +
				"Commit or stash the following before running inspector:\n\n" + status,
		}, nil
	}

	commit, err := HeadCommit(repoRoot)
	if err != nil {
		return Result{}, err
	}

	if opts.ExpectCommit != "" {
		resolved, err := ResolveCommit(repoRoot, opts.ExpectCommit)
		if err != nil {
			return Result{
				Outcome: Refused,
				Commit:  commit,
				Message: fmt.Sprintf("--commit %q does not resolve to exactly one commit: %v", opts.ExpectCommit, err),
			}, nil
		}
		if resolved != commit {
			return Result{
				Outcome: Refused,
				Commit:  commit,
				Message: fmt.Sprintf("expected commit %s (resolved from %q) but HEAD is %s - refusing rather than inspecting the wrong commit.", resolved, opts.ExpectCommit, commit),
			}, nil
		}
	}

	cfg, err := LoadConfig(repoRoot)
	if errors.Is(err, ErrNoCheckCommand) {
		return Result{
			Outcome: Refused,
			Commit:  commit,
			Message: fmt.Sprintf(
				"no check command configured for this repo.\n\n"+
					"inspector refuses to run rather than silently doing nothing - a repo that runs\n"+
					"unprotected must never look identical to one that runs protected.\n\n"+
					"Configure it by creating %s in the repo root:\n\n"+
					"  {\n    \"check\": \"<your project's own test/lint/build command>\"\n  }\n",
				ConfigFileName,
			),
		}, nil
	}
	if err != nil {
		return Result{Outcome: Refused, Commit: commit, Message: err.Error()}, nil
	}

	started := time.Now()
	checkResult, err := RunCheck(repoRoot, cfg.Check, opts.Stdout, opts.Stderr)
	if err != nil {
		return Result{}, fmt.Errorf("running check command: %w", err)
	}
	finished := time.Now()

	outcome := Green
	if checkResult.ExitCode != 0 {
		outcome = Red
	}

	report := Report{
		Commit:       commit,
		Repo:         repoRoot,
		Claim:        opts.Claim,
		CheckCommand: cfg.Check,
		Outcome:      outcome,
		StartedAt:    started,
		FinishedAt:   finished,
		DurationMS:   finished.Sub(started).Milliseconds(),
		ExitCode:     checkResult.ExitCode,
		Output:       checkResult.Output,
	}

	reportPath, err := WriteReport(repoRoot, report)
	if err != nil {
		return Result{}, err
	}

	return Result{
		Outcome:    outcome,
		Commit:     commit,
		ReportPath: reportPath,
	}, nil
}
