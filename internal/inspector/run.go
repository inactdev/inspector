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
	// Refused means inspector never reached a verdict on the code - no
	// check command configured, a dirty working tree, a check command
	// killed by a signal before it could finish on its own, or a check
	// command that ran past its timeout and was killed for it. Those
	// last two did run, unlike the others: something (the OOM killer,
	// an external kill, inspector's own deadline) killed it before it
	// judged the code at all, so its exit status is not a verdict on
	// the code either way. A refusal is loud on purpose: it must never
	// look like a pass.
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

	Stdout io.Writer
	Stderr io.Writer
}

// Result is the outcome of a Run.
type Result struct {
	Outcome Outcome
	Commit  string
	Message string // human-readable explanation; always set for Refused
	// ReportPath is set whenever the check actually ran and its report
	// saved successfully - for Green, Red, and a signal-killed Refused,
	// but not for a Refused that never ran a check at all, and not when
	// Warning is set (the save itself is what failed).
	ReportPath string
	// Warning is set when the check reached a real verdict (Green or
	// Red) but something worth loudly flagging happened alongside it -
	// currently, only that the local report could not be saved. The
	// commit status, not this local report, is the authority (SPEC.md),
	// so a save failure must never downgrade or discard the verdict
	// itself.
	Warning string
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

	timeout := cfg.Timeout()
	started := time.Now()
	checkResult, err := RunCheck(repoRoot, cfg.Check, timeout, opts.Stdout, opts.Stderr)
	if err != nil {
		return Result{}, fmt.Errorf("running check command: %w", err)
	}
	finished := time.Now()

	outcome := Green
	switch {
	case checkResult.TimedOut, checkResult.Signal != "":
		outcome = Refused
	case checkResult.ExitCode != 0:
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
		Signal:       checkResult.Signal,
		TimedOut:     checkResult.TimedOut,
		Output:       checkResult.Output,
	}

	result := Result{Outcome: outcome, Commit: commit}
	reportPath, writeErr := WriteReport(repoRoot, report)

	switch {
	case checkResult.TimedOut:
		result.Message = fmt.Sprintf(
			"the check command did not finish within its %s timeout and was killed, along with anything it started - inspector never reached a verdict, so this cannot be a verdict. Set timeoutSeconds in %s if this project's checks legitimately need longer.",
			timeout, ConfigFileName,
		)
		if writeErr != nil {
			result.Message += fmt.Sprintf(" Its local report also failed to save: %v", writeErr)
		} else {
			result.ReportPath = reportPath
		}
	case checkResult.Signal != "":
		result.Message = fmt.Sprintf(
			"the check command's process ended abnormally (%s) before it finished - inspector never judged the code, so this cannot be a verdict.",
			checkResult.Signal,
		)
		if writeErr != nil {
			result.Message += fmt.Sprintf(" Its local report also failed to save: %v", writeErr)
		} else {
			result.ReportPath = reportPath
		}
	case writeErr != nil:
		// outcome is Green or Red here - a real verdict already reached.
		// Discarding it because its notes failed to save would lose the
		// answer over losing the footnote; the commit status, not this
		// file, is authoritative (SPEC.md), so the verdict stands
		// regardless and the failure is surfaced as a loud warning
		// instead.
		result.Warning = fmt.Sprintf(
			"the %s verdict above is real, but its local report could not be saved: %v\n"+
				"the report is notes only, never authority - the verdict stands regardless.",
			outcome, writeErr,
		)
	default:
		result.ReportPath = reportPath
	}

	return result, nil
}
