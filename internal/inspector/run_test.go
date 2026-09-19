package inspector

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inactdev/inspector/internal/container"
)

func runOpts(dir string) (Options, *bytes.Buffer, *bytes.Buffer) {
	var stdout, stderr bytes.Buffer
	return Options{RepoPath: dir, Stdout: &stdout, Stderr: &stderr}, &stdout, &stderr
}

func TestRun_Green(t *testing.T) {
	requireDocker(t)
	dir := newTestRepo(t, map[string]string{
		ConfigFileName: `{"check": "true", "image": "alpine"}`,
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
	wantBranch := strings.TrimSpace(runGitT(t, dir, "branch", "--show-current"))
	if result.Branch != wantBranch {
		t.Fatalf("Branch = %q, want handed-off branch %q", result.Branch, wantBranch)
	}
}

func TestRun_Red(t *testing.T) {
	requireDocker(t)
	dir := newTestRepo(t, map[string]string{
		ConfigFileName: `{"check": "false", "image": "alpine"}`,
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
		ConfigFileName: `{"check": "sleep 30", "image": "alpine", "timeoutSeconds": 1}`,
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
		ConfigFileName: `{"check": "sleep 30", "image": "alpine", "timeoutSeconds": 1}`,
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
		ConfigFileName: `{"check": "true", "image": "alpine"}`,
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
		ConfigFileName: `{"check": "sh -c 'kill -9 $$' && true", "image": "alpine"}`,
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
		ConfigFileName: `{"check": "true", "image": "alpine"}`,
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
		ConfigFileName: `{"check": "true", "image": "alpine"}`,
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

func TestRun_RefusesWithoutImage(t *testing.T) {
	dir := newTestRepo(t, map[string]string{
		ConfigFileName: `{"check": "true"}`,
	})
	opts, _, _ := runOpts(dir)

	result, err := Run(opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != Refused {
		t.Fatalf("Outcome = %v, want Refused", result.Outcome)
	}
	if !strings.Contains(result.Message, "image") {
		t.Fatalf("refusal message should mention the missing image, got: %s", result.Message)
	}
	if !strings.Contains(result.Message, ConfigFileName) {
		t.Fatalf("refusal message should name %s, got: %s", ConfigFileName, result.Message)
	}
}

func TestRun_RefusesWithoutContainerRuntime(t *testing.T) {
	// Never silently fall back to running on the host - a missing
	// runtime must refuse loudly and name what's missing. Run still
	// needs git to resolve the repo and its commit before it ever gets
	// to the container-availability check, so the reduced PATH below
	// keeps a git symlink and drops everything else - docker included.
	dir := newTestRepo(t, map[string]string{
		ConfigFileName: `{"check": "true", "image": "alpine"}`,
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
		ConfigFileName: `{"check": "true", "image": "alpine"}`,
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
		ConfigFileName: `{"check": "true", "image": "alpine"}`,
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
