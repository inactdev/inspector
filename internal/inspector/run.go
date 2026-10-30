// Package inspector runs a project's own check command against one exact
// commit and reports green or red. See SPEC.md for the design this
// implements.
package inspector

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/inactdev/inspector/internal/container"
)

// Outcome is the result of one inspector run.
type Outcome string

const (
	// Green means the project's check command passed.
	Green Outcome = "green"
	// Red means the project's check command failed.
	Red Outcome = "red"
	// Refused means inspector never reached a verdict on the code - no
	// check command configured, a retired config key still set, a
	// -terms folder that cannot be used as this project's terms, no
	// committed Dockerfile to build the check image from or an image
	// that would not build, no usable container runtime, a
	// repo whose git directory cannot be reached inside the container or
	// whose source cannot be mounted read-only, a
	// dirty working tree, a check command killed by a signal before it
	// could finish on its own, or a check command that ran past its
	// timeout and was killed for it. Those
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
	// TermsDir is the folder holding this project's terms -
	// ConfigFileName, ProjectDockerfileName, and the check script the
	// first of those names - for a repository that commits none of them
	// (issue #69). Empty, which is the ordinary case, reads them from
	// the repository root. See ResolveTerms and Terms.
	TermsDir string
	// Claim is free-text context for what's being claimed as finished.
	// It is recorded in the local report only - inspector does not act
	// on its content.
	Claim string
	// NoStamp records that this run was told to publish a green result
	// without posting any commit status (issue #69). Run neither
	// publishes nor stamps anything itself - it carries the flag into
	// the local report, so a run whose stamp was skipped on purpose and
	// one whose stamp was never confirmed cannot read the same way
	// afterwards.
	NoStamp bool

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
	// TestPaths carries the project's own testPaths patterns through to
	// publication, so the caller that posts TestsStatusContext reads them
	// from the same config load the check itself ran under rather than
	// re-reading .inspector.json after the check (issue #44).
	TestPaths []string
	// Warning is set when the check reached a real verdict (Green or
	// Red) but something worth loudly flagging happened alongside it -
	// currently, that the local report could not be saved, or that it
	// was saved but older reports could not be pruned. Green
	// publication and the exit code, not this local report, carry the
	// verdict, so a save failure must never downgrade or discard it.
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

	// After the clean-tree refusal above, which is about the repository
	// and stays about the repository: a terms folder is configuration
	// handed to this run, not the thing being judged (issue #69).
	terms, err := ResolveTerms(repoRoot, opts.TermsDir)
	if err != nil {
		return Result{
			Outcome: Refused,
			Commit:  commit,
			Message: fmt.Sprintf(
				"%v\n\n"+
					"inspector reads this project's terms - its %s, its %s, and the\n"+
					"check script they name - from that folder, and refuses rather than falling back\n"+
					"to the repository's own: a run told to judge a project by one set of terms must\n"+
					"never judge it by another.",
				err, ConfigFileName, ProjectDockerfileName,
			),
		}, nil
	}

	cfg, err := terms.Config()
	if errors.Is(err, ErrNoCheckCommand) {
		return Result{
			Outcome: Refused,
			Commit:  commit,
			Message: fmt.Sprintf(
				"no check command configured for this repo.\n\n"+
					"inspector refuses to run rather than silently doing nothing - a repo that runs\n"+
					"unprotected must never look identical to one that runs protected.\n\n"+
					"Configure it by creating %s in %s:\n\n"+
					"  {\n    \"check\": \"<your project's own test/lint/build command>\"\n  }\n\n"+
					"and a %s beside it with your project's toolchain and\n"+
					"dependencies in it - that is the image the check command runs in.\n",
				ConfigFileName, terms.Location(), ProjectDockerfileName,
			),
		}, nil
	}
	if errors.Is(err, ErrRetiredImageKey) {
		return Result{
			Outcome: Refused,
			Commit:  commit,
			Message: fmt.Sprintf(
				"%s still names an \"image\".\n\n"+
					"the check command's image is no longer configured by name: it is built from\n"+
					"the %s this project commits, so that the box the work is written\n"+
					"in and the box it is judged in cannot differ. Remove the \"image\" field and\n"+
					"commit a %s whose FROM names the image it used to name.\n\n"+
					"inspector refuses rather than ignoring the field, because a config naming one\n"+
					"image while the check runs in another says something untrue about this run.\n",
				ConfigFileName, ProjectDockerfileName, ProjectDockerfileName,
			),
		}, nil
	}
	if errors.Is(err, ErrRetiredNetworkKey) {
		return Result{
			Outcome: Refused,
			Commit:  commit,
			Message: fmt.Sprintf(
				"%s still carries the \"network\" opt-in.\n\n"+
					"that opt-in is gone, as every run that used it said it would be: the check\n"+
					"container now always runs with no network at all. Whatever it was installing\n"+
					"at check time belongs in the %s this project commits, baked in\n"+
					"at build time - the build may use the network, the check may not. Remove the\n"+
					"\"network\" field.\n",
				ConfigFileName, ProjectDockerfileName,
			),
		}, nil
	}
	if err != nil {
		return Result{Outcome: Refused, Commit: commit, Message: err.Error()}, nil
	}

	// Resolved before the runtime check, because it needs no runtime: a
	// project that commits no image to build from has a gap in what it
	// declares, and saying so is more use than reporting a missing
	// Docker to someone who would hit this immediately afterwards.
	dockerfile, err := terms.Dockerfile()
	if refusal, refused := refuseProjectImage(repoRoot, commit, terms, container.ProjectImage{}, err); refused {
		return refusal, nil
	}

	if err := container.EnsureAvailable(); err != nil {
		return Result{
			Outcome: Refused,
			Commit:  commit,
			Message: fmt.Sprintf(
				"no usable container runtime: %v\n\n"+
					"inspector runs every check command inside a container instead of on this\n"+
					"machine directly, and refuses rather than silently running unsandboxed.",
				err,
			),
		}, nil
	}

	// Built before the check and outside its timeout: the image is the
	// project's own declaration of the environment it is judged in, and
	// a project whose image inspector cannot produce has not been judged
	// at all. Docker's output goes straight to the caller's stderr as it
	// happens, so a long first build is visible rather than silent.
	buildLog := opts.Stderr
	if buildLog == nil {
		// os/exec reads a nil writer as /dev/null, but the build streams
		// through writers of its own that do not.
		buildLog = io.Discard
	}
	// The build context is the directory the Dockerfile lives in, which
	// is the repository root unless -terms moved the terms out of it.
	image, err := BuildProjectImage(terms.Dir, dockerfile, buildLog)
	if refusal, refused := refuseProjectImage(repoRoot, commit, terms, image, err); refused {
		return refusal, nil
	}
	fmt.Fprintln(buildLog, DescribeProjectImage(image))

	timeout := cfg.Timeout()
	started := time.Now()
	checkResult, err := RunCheck(repoRoot, cfg.Check, image.Tag, terms.Mount(), cfg.Limits(), timeout, opts.Stdout, opts.Stderr)
	if errors.Is(err, container.ErrGitDirUnavailable) {
		return Result{
			Outcome: Refused,
			Commit:  commit,
			Message: fmt.Sprintf(
				"%v\n\n"+
					"inspector mounts that directory into the container read-only, at its own\n"+
					"path, so a check command can use git from a linked worktree - but it could\n"+
					"not this time, and a check command whose every git call fails for a reason\n"+
					"that has nothing to do with the code is not a verdict on the code.",
				err,
			),
		}, nil
	}
	if errors.Is(err, container.ErrWorkspaceUnavailable) {
		return Result{
			Outcome: Refused,
			Commit:  commit,
			Message: fmt.Sprintf(
				"%v\n\n"+
					"the commit under check is mounted read-only, and the check command writes to a\n"+
					"throwaway workspace laid under it instead, so it cannot modify the code it is\n"+
					"being judged on. inspector refuses rather than running a check whose source it\n"+
					"could not make read-only - a check that can rewrite its own source can pass\n"+
					"against bytes that exist nowhere.",
				err,
			),
		}, nil
	}
	if errors.Is(err, container.ErrRemoteDocker) {
		return Result{
			Outcome: Refused,
			Commit:  commit,
			Message: fmt.Sprintf(
				"%v\n\n"+
					"inspector runs the check command as the inviting host user instead of the\n"+
					"image's own default, so a runaway check cannot own anything on the host it\n"+
					"didn't already own - but that only means something when the container and\n"+
					"this machine share one filesystem and user namespace, which a remote Docker\n"+
					"endpoint does not.",
				err,
			),
		}, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("running check command: %w", err)
	}
	// The check command's writable workspace is kept until this run's
	// report is written and discarded after it, by ruling (issue #27):
	// whatever the check installed or built belongs to that one run, and
	// nothing in it is evidence once the report exists.
	defer checkResult.Workspace.Cleanup()
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
		// Only when they came from outside the repository: a report that
		// named the repo root as its terms folder on every ordinary run
		// would say nothing, while this says which terms judged the
		// commit in the one case where that is not obvious.
		TermsDir:   termsDirReported(terms),
		NoStamp:    opts.NoStamp,
		Image:      image.Tag,
		ImageBases: imageBases(image),
		Outcome:    outcome,
		StartedAt:  started,
		FinishedAt: finished,
		DurationMS: finished.Sub(started).Milliseconds(),
		ExitCode:   checkResult.ExitCode,
		Signal:     checkResult.Signal,
		TimedOut:   checkResult.TimedOut,
		Output:     checkResult.Output,
		// Recorded as a fact of the run, not left for a reader to infer
		// from the output's size: a report that silently holds part of
		// the output is worse than a large one (issue #12).
		OutputTruncated:    checkResult.OutputDroppedBytes > 0,
		OutputDroppedBytes: checkResult.OutputDroppedBytes,
		OutputTotalBytes:   checkResult.OutputTotalBytes,
	}

	result := Result{Outcome: outcome, Commit: commit, TestPaths: cfg.TestPaths}
	reportPath, writeErr := WriteReport(repoRoot, report)
	if errors.Is(writeErr, ErrRunsNotPruned) {
		// This run's own report was written; what failed is the bound on
		// the directory holding the older ones. That is worth saying
		// loudly and worth nothing else: the result stands, and
		// reportPath still names a real file.
		result.Warning = fmt.Sprintf(
			"%v\n%s may now hold more than the %d runs it keeps, so it can grow until they are removed by hand.\n"+
				"this run's own report was saved and is unaffected.",
			writeErr, filepath.Join(RunsDirName, "runs"), MaxRunsKept,
		)
		writeErr = nil
	}

	switch {
	case checkResult.TimedOut && checkResult.KillFailed:
		result.Message = fmt.Sprintf(
			"the check command did not finish within its %s timeout, and docker refused to stop its container - inspector cannot confirm it was stopped, so it may still be running against this repo (check `docker ps`; its own output above says what docker reported). inspector never reached a verdict, so this cannot be a verdict. Set timeoutSeconds in %s if this project's checks legitimately need longer.",
			timeout, ConfigFileName,
		)
		if writeErr != nil {
			result.Message += fmt.Sprintf(" Its local report also failed to save: %v", writeErr)
		} else {
			result.ReportPath = reportPath
		}
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
		// outcome is Green or Red here - a real local check result.
		// Discarding it because its notes failed to save would lose the
		// answer over losing the footnote; this file is never authority,
		// so the result stands and the failure is surfaced as a loud
		// warning instead.
		result.Warning = fmt.Sprintf(
			"the local %s check result is real, but its report could not be saved: %v\n"+
				"the report is notes only, never authority - publication and the final exit code still determine the verdict.",
			outcome, writeErr,
		)
	default:
		result.ReportPath = reportPath
	}

	return result, nil
}

// refuseProjectImage turns every way the project's own check image can
// fail to be produced into a refusal, and reports false when there was no
// failure. None of these is a red: a project whose declared environment
// inspector could not build has not been judged, and judging it in some
// other image would judge a different project (issue #47).
func refuseProjectImage(repoRoot, commit string, terms Terms, image container.ProjectImage, err error) (Result, bool) {
	switch {
	case err == nil:
		return Result{}, false
	case errors.Is(err, ErrNoProjectImage):
		return Result{
			Outcome: Refused,
			Commit:  commit,
			Message: fmt.Sprintf(
				"there is no %s in %s, so there is no image to run this project's check in.\n\n"+
					"inspector builds that file and runs the check command inside the result - it\n"+
					"never guesses at a toolchain, the same way it never guesses at a check\n"+
					"command. Add %s in %s, carrying this project's\n"+
					"toolchain and its dependencies:\n\n"+
					"  FROM <a base image with your toolchain>\n"+
					"  # install this project's dependencies here, so the check needs no network\n\n"+
					"The same image is meant to be the box the work is written in, so what the\n"+
					"check runs against cannot differ from what the work was built against.\n",
				ProjectDockerfileName, terms.Location(), ProjectDockerfileName, terms.Location(),
			),
		}, true
	case errors.Is(err, ErrUncommittedProjectImage):
		return Result{
			Outcome: Refused,
			Commit:  commit,
			Message: fmt.Sprintf(
				"%s exists here but git does not track it (it is ignored, or was\n"+
					"never added), so the image the check would run in is not part of this commit\n"+
					"and nobody reviewing this work would ever see it. Commit it.\n",
				ProjectDockerfileName,
			),
		}, true
	case errors.Is(err, container.ErrProjectImageBuildFailed):
		return Result{
			Outcome: Refused,
			Commit:  commit,
			Message: fmt.Sprintf(
				"%v\n\n"+
					"the check command runs in the image this project's own %s\n"+
					"builds, and that build failed - docker's own output is above%s. Nothing was\n"+
					"judged: a project whose check environment does not build has not been found\n"+
					"wrong about its code.\n",
				err, ProjectDockerfileName, buildLogSuffix(repoRoot, image),
			),
		}, true
	case errors.Is(err, container.ErrProjectImageUnavailable):
		return Result{
			Outcome: Refused,
			Commit:  commit,
			Message: fmt.Sprintf(
				"%v\n\n"+
					"inspector reads this project's own %s to know which image to\n"+
					"build and tag for the check, and could not. It refuses rather than falling\n"+
					"back to an image nobody declared.\n",
				err, ProjectDockerfileName,
			),
		}, true
	default:
		return Result{Outcome: Refused, Commit: commit, Message: err.Error()}, true
	}
}

// buildLogSuffix keeps a failed build's own log, which is the only thing
// that says why it failed, and names where it was kept. The log is already
// streamed live, but a Fabrica handoff reads a refusal message rather than
// a console, so the file is what a human is pointed at afterwards.
func buildLogSuffix(repoRoot string, image container.ProjectImage) string {
	if strings.TrimSpace(image.Log) == "" {
		return ""
	}
	path, err := WriteImageBuildLog(repoRoot, image.Dockerfile, image.Log)
	if err != nil {
		return fmt.Sprintf(" (its log could not be saved: %v)", err)
	}
	return fmt.Sprintf(" and kept in %s", path)
}

// termsDirReported is the terms folder a report names, and nothing for
// the ordinary run whose terms are the repository's own - those are in
// the commit the report already names.
func termsDirReported(terms Terms) string {
	if !terms.Outside {
		return ""
	}
	return terms.Dir
}

// imageBases records which base image each FROM resolved to, so the
// report says exactly which environment judged this commit rather than
// only naming the derived tag.
func imageBases(image container.ProjectImage) map[string]string {
	if len(image.Bases) == 0 {
		return nil
	}
	bases := make(map[string]string, len(image.Bases))
	for _, base := range image.Bases {
		bases[base.Ref] = base.ID
	}
	return bases
}
