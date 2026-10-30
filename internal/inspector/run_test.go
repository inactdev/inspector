package inspector

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inactdev/inspector/internal/container"
	"github.com/inactdev/inspector/internal/dockertest"
)

func runOpts(dir string) (Options, *bytes.Buffer, *bytes.Buffer) {
	var stdout, stderr bytes.Buffer
	return Options{RepoPath: dir, Stdout: &stdout, Stderr: &stderr}, &stdout, &stderr
}

func TestRun_Green(t *testing.T) {
	requireDocker(t)
	dir := newTestRepo(t, map[string]string{
		ConfigFileName: `{"check": "true"}`,
	})
	opts, _, _ := runOpts(dir)

	result, err := Run(opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != Green {
		t.Fatalf("Outcome = %v, want Green (message: %s)", result.Outcome, result.Message)
	}
	if result.ReportPath == "" {
		t.Fatal("expected a report path for a green run")
	}
	if result.Commit == "" {
		t.Fatal("expected a commit for a green run")
	}
}

func TestRun_Red(t *testing.T) {
	requireDocker(t)
	dir := newTestRepo(t, map[string]string{
		ConfigFileName: `{"check": "false"}`,
	})
	opts, _, _ := runOpts(dir)

	result, err := Run(opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != Red {
		t.Fatalf("Outcome = %v, want Red", result.Outcome)
	}
	if result.ReportPath == "" {
		t.Fatal("expected a report path for a red run")
	}
}

func TestRun_TimedOutCheckIsRefusedNotRed(t *testing.T) {
	requireDocker(t)
	dir := newTestRepo(t, map[string]string{
		ConfigFileName: `{"check": "sleep 30", "timeoutSeconds": 1}`,
	})
	opts, _, _ := runOpts(dir)

	result, err := Run(opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != Refused {
		t.Fatalf("Outcome = %v, want Refused (a timed-out check never reached a verdict) - message: %s", result.Outcome, result.Message)
	}
	if !strings.Contains(result.Message, "timeout") {
		t.Fatalf("message should mention the timeout, got: %s", result.Message)
	}
	if !strings.Contains(result.Message, "was killed") {
		t.Fatalf("a kill docker accepted should still be described as one, got: %s", result.Message)
	}
	if result.ReportPath == "" {
		t.Fatal("expected a report path - the check did run and its output is worth keeping")
	}
	data := readFile(t, result.ReportPath)
	if !strings.Contains(data, `"timedOut": true`) {
		t.Fatalf("report should record timedOut, got: %s", data)
	}
}

func TestRun_TimedOutCheckWhoseContainerDockerRefusedToStop(t *testing.T) {
	// The refusal a person reads has to match what actually happened: a
	// container docker would not stop may still be running against the
	// repo, so claiming it was killed would be a lie in exactly the
	// situation where knowing the truth matters.
	requireDocker(t)
	dir := newTestRepo(t, map[string]string{
		ConfigFileName: `{"check": "sleep 30", "timeoutSeconds": 1}`,
	})
	failingDockerKill(t)
	opts, _, stderr := runOpts(dir)

	result, err := Run(opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != Refused {
		t.Fatalf("Outcome = %v, want Refused - message: %s", result.Outcome, result.Message)
	}
	if strings.Contains(result.Message, "was killed") {
		t.Fatalf("message claims the check was killed when docker refused to stop it: %s", result.Message)
	}
	if !strings.Contains(result.Message, "may still be running") {
		t.Fatalf("message should say the container may still be running, got: %s", result.Message)
	}
	if !strings.Contains(stderr.String(), "could not stop the check container") {
		t.Fatalf("stderr should carry the live warning, got: %s", truncate(stderr.String(), 400))
	}
	if result.ReportPath == "" {
		t.Fatal("expected a report path - a refused kill must not cost the run its report")
	}
	data := readFile(t, result.ReportPath)
	if !strings.Contains(data, "is not running") {
		t.Fatalf("report should keep docker's own refusal, got: %s", truncate(data, 600))
	}
}

func TestRun_DefaultTimeoutAppliesWhenUnconfigured(t *testing.T) {
	requireDocker(t)
	dir := newTestRepo(t, map[string]string{
		ConfigFileName: `{"check": "true"}`,
	})
	opts, _, _ := runOpts(dir)

	result, err := Run(opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != Green {
		t.Fatalf("Outcome = %v, want Green - the default timeout must not fire on a fast check", result.Outcome)
	}
}

func TestRun_SignalKilledCheckIsRefusedNotRed(t *testing.T) {
	// A container's own init process is immune to a signal sent to it
	// from within its own PID namespace, even SIGKILL - a real Linux
	// pid-namespace behavior, not a bug (see AGENTS.md) - so this has to
	// kill a forked child rather than the check command's own top-level
	// shell for the signal to actually take effect. That also makes it
	// the motivating case for Refused rather than Red: a compound check
	// command shaped like README's own example (`npm test && npm run
	// lint`) can survive its own killed child and exit 137, a value
	// nothing prevents a program from also choosing on its own - the
	// client's ruling is to call that Refused anyway, since that
	// direction is safe (Refused still blocks a merge) while the other
	// direction sends someone hunting a bug that was never there when
	// the machine ran out of memory.
	requireDocker(t)
	dir := newTestRepo(t, map[string]string{
		ConfigFileName: `{"check": "sh -c 'kill -9 $$' && true"}`,
	})
	opts, _, _ := runOpts(dir)

	result, err := Run(opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != Refused {
		t.Fatalf("Outcome = %v, want Refused (a signal-killed check never judged the code) - message: %s", result.Outcome, result.Message)
	}
	if !strings.Contains(result.Message, "killed") {
		t.Fatalf("message should describe the signal, got: %s", result.Message)
	}
	if result.ReportPath == "" {
		t.Fatal("expected a report path - the check did run and its output is worth keeping")
	}
	data := readFile(t, result.ReportPath)
	if !strings.Contains(data, `"outcome": "refused"`) {
		t.Fatalf("report should record the refused outcome, got: %s", data)
	}
}

func TestRun_ReportWriteFailureKeepsVerdict(t *testing.T) {
	requireDocker(t)
	dir := newTestRepo(t, map[string]string{
		ConfigFileName: `{"check": "true"}`,
	})
	// Occupy RunsDirName with a plain file, so WriteReport's MkdirAll
	// fails - portable across privilege levels, unlike a permission-bit
	// test that root would sail through.
	writeFiles(t, dir, map[string]string{RunsDirName: "occupied"})

	opts, _, _ := runOpts(dir)
	result, err := Run(opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != Green {
		t.Fatalf("Outcome = %v, want Green - a reached verdict must survive a report-write failure", result.Outcome)
	}
	if result.Warning == "" {
		t.Fatal("expected a Warning describing the report-write failure")
	}
	if !strings.Contains(result.Warning, RunsDirName) {
		t.Fatalf("Warning should say where it failed, got: %s", result.Warning)
	}
	if result.ReportPath != "" {
		t.Fatalf("ReportPath should be empty when the report failed to save, got %q", result.ReportPath)
	}
}

func TestRun_ConsecutiveRunsBothSucceed(t *testing.T) {
	// Regression test: a repo that never gitignores .inspector/ must not
	// go permanently dirty - and therefore permanently refused - the
	// moment inspector writes its first report.
	requireDocker(t)
	dir := newTestRepo(t, map[string]string{
		ConfigFileName: `{"check": "true"}`,
	})

	for i := 0; i < 2; i++ {
		opts, _, _ := runOpts(dir)
		result, err := Run(opts)
		if err != nil {
			t.Fatalf("run %d: unexpected error: %v", i, err)
		}
		if result.Outcome != Green {
			t.Fatalf("run %d: Outcome = %v, want Green (message: %s)", i, result.Outcome, result.Message)
		}
	}
}

func TestRun_RefusesWithoutConfig(t *testing.T) {
	dir := newTestRepo(t, nil)
	opts, _, _ := runOpts(dir)

	result, err := Run(opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != Refused {
		t.Fatalf("Outcome = %v, want Refused", result.Outcome)
	}
	if !strings.Contains(result.Message, ConfigFileName) {
		t.Fatalf("refusal message should name %s, got: %s", ConfigFileName, result.Message)
	}
	if result.ReportPath != "" {
		t.Fatal("a refusal should not write a report")
	}
}

// TestRun_RefusesWithoutACommittedDockerfile is the issue #47
// requirement: a project with no committed image to build from is
// refused, by the name of the file it has to add. No docker needed - this
// refusal lands before any container work.
func TestRun_RefusesWithoutACommittedDockerfile(t *testing.T) {
	dir := newTestRepo(t, map[string]string{
		ConfigFileName: `{"check": "true"}`,
	})
	runGitT(t, dir, "rm", "-q", ProjectDockerfileName)
	runGitT(t, dir, "commit", "-q", "-m", "drop the check image")
	opts, _, _ := runOpts(dir)

	result, err := Run(opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != Refused {
		t.Fatalf("Outcome = %v, want Refused", result.Outcome)
	}
	if !strings.Contains(result.Message, ProjectDockerfileName) {
		t.Fatalf("refusal message should name the file to add, got: %s", result.Message)
	}
	if result.ReportPath != "" {
		t.Fatal("a refusal that never ran a check should not write a report")
	}
}

// A Dockerfile that is present but gitignored is the one way a file can
// survive the clean-tree check without being in the commit, which would
// leave the image the check ran in invisible to anyone reviewing the work.
func TestRun_RefusesAnUncommittedDockerfile(t *testing.T) {
	dir := newTestRepo(t, map[string]string{
		ConfigFileName: `{"check": "true"}`,
		".gitignore":   ProjectDockerfileName + "\n",
	})
	opts, _, _ := runOpts(dir)

	result, err := Run(opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != Refused {
		t.Fatalf("Outcome = %v, want Refused", result.Outcome)
	}
	if !strings.Contains(result.Message, "does not track it") {
		t.Fatalf("refusal message should say the file is not committed, got: %s", result.Message)
	}
}

// Both retired keys are refused rather than ignored, naming the file that
// replaced them (issue #47). A config that still carries one describes a
// run inspector will not perform.
func TestRun_RefusesRetiredConfigKeys(t *testing.T) {
	cases := map[string]struct {
		config string
		want   string
	}{
		"image":   {`{"check": "true", "image": "alpine"}`, "\"image\""},
		"network": {`{"check": "true", "network": true}`, "\"network\""},
	}
	for name, tc := range cases {
		dir := newTestRepo(t, map[string]string{ConfigFileName: tc.config})
		opts, _, _ := runOpts(dir)

		result, err := Run(opts)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", name, err)
		}
		if result.Outcome != Refused {
			t.Fatalf("%s: Outcome = %v, want Refused", name, result.Outcome)
		}
		if !strings.Contains(result.Message, tc.want) || !strings.Contains(result.Message, ProjectDockerfileName) {
			t.Fatalf("%s: refusal should name the retired key and the file that replaced it, got: %s", name, result.Message)
		}
	}
}

// A Dockerfile that does not build is a refusal with its log kept, never
// a red: a project whose check environment will not build has not been
// found wrong about its code.
func TestRun_RefusesWhenTheCheckImageDoesNotBuild(t *testing.T) {
	requireDocker(t)
	dir := newTestRepo(t, map[string]string{
		ConfigFileName:        `{"check": "true"}`,
		ProjectDockerfileName: "FROM " + testImage + "\nRUN echo this-build-fails-on-purpose >&2; exit 3\n",
	})
	opts, _, _ := runOpts(dir)

	result, err := Run(opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != Refused {
		t.Fatalf("Outcome = %v, want Refused (message: %s)", result.Outcome, result.Message)
	}
	if !strings.Contains(result.Message, ProjectDockerfileName) {
		t.Fatalf("refusal should name the file that failed to build, got: %s", result.Message)
	}
	logPath := filepath.Join(dir, RunsDirName, ImageBuildLogName)
	if !strings.Contains(result.Message, logPath) {
		t.Fatalf("refusal should name where the build log was kept, got: %s", result.Message)
	}
	if !strings.Contains(readFile(t, logPath), "this-build-fails-on-purpose") {
		t.Fatalf("%s should hold docker's own output for the failed build", logPath)
	}
}

// The report names the image the check ran in, so a reader can tell which
// environment judged this commit rather than only which command ran.
func TestRun_ReportNamesTheCheckImage(t *testing.T) {
	requireDocker(t)
	dir := newTestRepo(t, map[string]string{ConfigFileName: `{"check": "true"}`})
	opts, _, _ := runOpts(dir)

	result, err := Run(opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != Green {
		t.Fatalf("Outcome = %v, want Green (message: %s)", result.Outcome, result.Message)
	}
	data := readFile(t, result.ReportPath)
	if !strings.Contains(data, container.ProjectImageRepository+":") {
		t.Fatalf("report should name the built check image, got: %s", data)
	}
	if !strings.Contains(data, testImage) {
		t.Fatalf("report should name the base image the check image was built from, got: %s", data)
	}
}

// newWorkModeRepo is the repository issue #69 exists for: one that
// carries nothing of inspector's at all - no config, and the default
// Dockerfile removed again - because nothing may be added to it.
func newWorkModeRepo(t *testing.T) string {
	t.Helper()
	dir := newTestRepo(t, map[string]string{"src/app.txt": "the company's own code\n"})
	runGitT(t, dir, "rm", "-q", ProjectDockerfileName)
	runGitT(t, dir, "commit", "-q", "-m", "a repository that carries no terms")
	return dir
}

// TestRun_TermsFromAFolderOutsideTheRepo is issue #69's main requirement:
// a repository that commits none of the three files is still judged, from
// a folder beside it, and the check command finds that folder read-only at
// container.TermsDir while the repository is still at WorkspaceDir.
func TestRun_TermsFromAFolderOutsideTheRepo(t *testing.T) {
	requireDocker(t)
	repo := newWorkModeRepo(t)
	terms := t.TempDir()
	writeFiles(t, terms, map[string]string{
		ConfigFileName:        `{"check": "sh ` + container.TermsDir + `/check.sh"}`,
		ProjectDockerfileName: defaultTestDockerfile,
		// Proves all four facts at once: the script came from the terms
		// folder, the repository is still at WorkspaceDir with its own
		// content, the folder is read-only in there, and the check runs
		// from the repository.
		"check.sh": "set -e\n" +
			"test \"$(pwd)\" = " + container.WorkspaceDir + "\n" +
			"cat src/app.txt\n" +
			"if touch " + container.TermsDir + "/written 2>/dev/null; then echo 'terms folder was writable'; exit 1; fi\n" +
			"echo check-script-from-the-terms-folder-ran\n",
	})
	opts, _, _ := runOpts(repo)
	opts.TermsDir = terms

	result, err := Run(opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != Green {
		t.Fatalf("Outcome = %v, want Green (message: %s)", result.Outcome, result.Message)
	}
	report := readFile(t, result.ReportPath)
	if !strings.Contains(report, "check-script-from-the-terms-folder-ran") {
		t.Fatalf("report should carry the terms folder's own check script output, got: %s", truncate(report, 800))
	}
	if !strings.Contains(report, "the company's own code") {
		t.Fatalf("the check command should still see the repository at %s, got: %s", container.WorkspaceDir, truncate(report, 800))
	}
	resolvedTerms, err := filepath.EvalSymlinks(terms)
	if err != nil {
		t.Fatalf("resolving the terms folder: %v", err)
	}
	if !strings.Contains(report, resolvedTerms) {
		t.Fatalf("report should name the terms folder that judged this commit, got: %s", truncate(report, 800))
	}
	// Nothing of inspector's reached the repository: that is the whole
	// point of the flag.
	if _, err := os.Stat(filepath.Join(repo, ConfigFileName)); !os.IsNotExist(err) {
		t.Fatalf("stat %s in the repository = %v, want it still absent", ConfigFileName, err)
	}
}

// The -terms folder is in no repository, so the one requirement that
// cannot apply to it - that the Dockerfile be committed - is skipped,
// while the repository's own clean-tree rule still decides the run.
func TestRun_TermsFolderNeedsNoCommitAndTheRepoStillMustBeClean(t *testing.T) {
	repo := newWorkModeRepo(t)
	terms := t.TempDir()
	writeFiles(t, terms, map[string]string{
		ConfigFileName:        `{"check": "true"}`,
		ProjectDockerfileName: defaultTestDockerfile,
	})
	writeFiles(t, repo, map[string]string{"dirty.txt": "uncommitted"})
	opts, _, _ := runOpts(repo)
	opts.TermsDir = terms

	result, err := Run(opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != Refused {
		t.Fatalf("Outcome = %v, want Refused - the repository's working tree still has to be clean", result.Outcome)
	}
	if !strings.Contains(result.Message, "dirty.txt") {
		t.Fatalf("refusal should name the dirty file in the repository, got: %s", result.Message)
	}
}

// Both terms the folder has to hold are refused by the name of the folder
// that does not hold them, not by the repo root that was never asked.
func TestRun_RefusesATermsFolderMissingItsFiles(t *testing.T) {
	cases := map[string]struct {
		files map[string]string
		want  string
	}{
		"no config":     {map[string]string{ProjectDockerfileName: defaultTestDockerfile}, ConfigFileName},
		"no dockerfile": {map[string]string{ConfigFileName: `{"check": "true"}`}, ProjectDockerfileName},
	}
	for name, tc := range cases {
		repo := newWorkModeRepo(t)
		terms := t.TempDir()
		writeFiles(t, terms, tc.files)
		opts, _, _ := runOpts(repo)
		opts.TermsDir = terms

		result, err := Run(opts)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", name, err)
		}
		if result.Outcome != Refused {
			t.Fatalf("%s: Outcome = %v, want Refused", name, result.Outcome)
		}
		// The resolved path, which is what a refusal names: the mount
		// source has to be the physical one (macOS's /var -> /private/var).
		terms, err = filepath.EvalSymlinks(terms)
		if err != nil {
			t.Fatalf("%s: resolving the terms folder: %v", name, err)
		}
		if !strings.Contains(result.Message, tc.want) || !strings.Contains(result.Message, terms) {
			t.Fatalf("%s: refusal should name the missing file and the terms folder, got: %s", name, result.Message)
		}
	}
}

func TestRun_RefusesAnUnusableTermsFolder(t *testing.T) {
	repo := newWorkModeRepo(t)
	opts, _, _ := runOpts(repo)
	opts.TermsDir = filepath.Join(t.TempDir(), "not-there")

	result, err := Run(opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != Refused {
		t.Fatalf("Outcome = %v, want Refused", result.Outcome)
	}
	if !strings.Contains(result.Message, "not-there") || !strings.Contains(result.Message, "refuses rather than falling back") {
		t.Fatalf("refusal should name the folder and say it will not fall back to the repo's own terms, got: %s", result.Message)
	}
	if result.ReportPath != "" {
		t.Fatal("a refusal that never ran a check should not write a report")
	}
}

// -no-stamp is a publication decision, so Run itself only records it -
// but it must record it, or a run that skipped its stamp on purpose reads
// afterwards like one whose stamp was never confirmed.
func TestRun_RecordsThatNoStampWasAsked(t *testing.T) {
	requireDocker(t)
	dir := newTestRepo(t, map[string]string{ConfigFileName: `{"check": "true"}`})
	opts, _, _ := runOpts(dir)
	opts.NoStamp = true

	result, err := Run(opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != Green {
		t.Fatalf("Outcome = %v, want Green (message: %s)", result.Outcome, result.Message)
	}
	if data := readFile(t, result.ReportPath); !strings.Contains(data, `"noStamp": true`) {
		t.Fatalf("report should record that no status was to be posted, got: %s", truncate(data, 600))
	}
}

func TestRun_RefusesWithoutContainerRuntime(t *testing.T) {
	// Never silently fall back to running on the host - a missing
	// runtime must refuse loudly and name what's missing. Run still
	// needs git to resolve the repo and its commit before it ever gets
	// to the container-availability check, so the reduced PATH below
	// keeps a git symlink and drops everything else - docker included.
	dir := newTestRepo(t, map[string]string{
		ConfigFileName: `{"check": "true"}`,
	})
	opts, _, _ := runOpts(dir)

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("finding git: %v", err)
	}
	binDir := t.TempDir()
	if err := os.Symlink(realGit, filepath.Join(binDir, "git")); err != nil {
		t.Fatalf("symlinking git: %v", err)
	}
	t.Setenv("PATH", binDir)

	result, err := Run(opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != Refused {
		t.Fatalf("Outcome = %v, want Refused", result.Outcome)
	}
	if !strings.Contains(result.Message, container.ErrNotInstalled.Error()) {
		t.Fatalf("refusal message should name the concrete missing requirement, got: %s", result.Message)
	}
}

func TestRun_RefusesOnDirtyWorkingTree(t *testing.T) {
	dir := newTestRepo(t, map[string]string{
		ConfigFileName: `{"check": "true"}`,
	})
	writeFiles(t, dir, map[string]string{"dirty.txt": "uncommitted"})
	opts, _, _ := runOpts(dir)

	result, err := Run(opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != Refused {
		t.Fatalf("Outcome = %v, want Refused", result.Outcome)
	}
	if !strings.Contains(result.Message, "dirty.txt") {
		t.Fatalf("refusal message should mention the dirty file, got: %s", result.Message)
	}
}

func TestRun_RefusesOnNonRepo(t *testing.T) {
	dir := t.TempDir()
	opts, _, _ := runOpts(dir)

	result, err := Run(opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != Refused {
		t.Fatalf("Outcome = %v, want Refused", result.Outcome)
	}
}

func TestRun_RecordsClaimInReport(t *testing.T) {
	requireDocker(t)
	dir := newTestRepo(t, map[string]string{
		ConfigFileName: `{"check": "true"}`,
	})
	opts, _, _ := runOpts(dir)
	opts.Claim = "implemented the login flow"

	result, err := Run(opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data := readFile(t, result.ReportPath)
	if !strings.Contains(data, "implemented the login flow") {
		t.Fatalf("report should record the claim, got: %s", data)
	}
}

func TestRun_GreenFromLinkedWorktreeWhoseCheckUsesGit(t *testing.T) {
	// The defect in issue #31: from a linked worktree the container saw
	// a .git pointer file naming a directory outside the mount, so every
	// git call a check made failed and the run came back red for a
	// reason that had nothing to do with the code.
	requireDocker(t)
	dockertest.RequireImage(t, gitTestImage)
	dir := newTestWorktree(t, map[string]string{
		ConfigFileName:        `{"check": "git log --oneline -1 && git status --porcelain"}`,
		ProjectDockerfileName: "FROM " + gitTestImage + "\n",
	})
	opts, _, _ := runOpts(dir)

	result, err := Run(opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != Green {
		t.Fatalf("Outcome = %v, want Green - a check command that uses git must work from a linked worktree (message: %s)", result.Outcome, result.Message)
	}
}

func TestRun_RefusesWhenTheGitDirCannotBeMadeAvailable(t *testing.T) {
	// The minimum acceptable behavior issue #31 names for any case that
	// cannot be supported: refuse by name, never report red on a check
	// that failed only because the repository was unreachable. Here the
	// shared config carries an [extensions] key the sanitizer will not
	// vouch for, so the mount cannot be built - while the host's own git
	// (which ignores unknown extensions at format version 0) works.
	requireDocker(t)
	dir := newTestWorktree(t, map[string]string{
		ConfigFileName: `{"check": "true"}`,
	})
	runGitT(t, dir, "config", "extensions.inspectorTest", "1")
	opts, _, _ := runOpts(dir)

	result, err := Run(opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != Refused {
		t.Fatalf("Outcome = %v, want Refused (message: %s)", result.Outcome, result.Message)
	}
	if !strings.Contains(result.Message, "linked git worktree and its git directory is outside the mount") {
		t.Fatalf("the refusal has to name what it is, got: %s", result.Message)
	}
	if !strings.Contains(result.Message, "extensions.inspectorTest") {
		t.Fatalf("the refusal has to name what it could not forward, got: %s", result.Message)
	}
}
