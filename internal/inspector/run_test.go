package inspector

import (
	"bytes"
	"strings"
	"testing"
)

func runOpts(dir string) (Options, *bytes.Buffer, *bytes.Buffer) {
	var stdout, stderr bytes.Buffer
	return Options{RepoPath: dir, Stdout: &stdout, Stderr: &stderr}, &stdout, &stderr
}

func TestRun_Green(t *testing.T) {
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

func TestRun_SignalKilledCheckIsRefusedNotRed(t *testing.T) {
	dir := newTestRepo(t, map[string]string{
		ConfigFileName: `{"check": "kill -9 $$"}`,
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

func TestRun_RefusesOnCommitMismatch(t *testing.T) {
	dir := newTestRepo(t, map[string]string{
		ConfigFileName: `{"check": "true"}`,
	})
	opts, _, _ := runOpts(dir)
	opts.ExpectCommit = "0000000000000000000000000000000000000dead"

	result, err := Run(opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != Refused {
		t.Fatalf("Outcome = %v, want Refused", result.Outcome)
	}
	if !strings.Contains(result.Message, "0000000000000000000000000000000000000dead") {
		t.Fatalf("refusal message should name the expected commit, got: %s", result.Message)
	}
}

func TestRun_CommitFlagAcceptsShortForm(t *testing.T) {
	dir := newTestRepo(t, map[string]string{ConfigFileName: `{"check": "true"}`})
	full := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	opts, _, _ := runOpts(dir)
	opts.ExpectCommit = full[:8]

	result, err := Run(opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != Green {
		t.Fatalf("Outcome = %v, want Green (message: %s)", result.Outcome, result.Message)
	}
}

func TestRun_CommitFlagRefusesShortFormMismatch(t *testing.T) {
	dir := newTestRepo(t, map[string]string{"a.txt": "hello"})
	firstCommit := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))

	writeFiles(t, dir, map[string]string{ConfigFileName: `{"check": "true"}`})
	runGitT(t, dir, "add", "-A")
	runGitT(t, dir, "commit", "-q", "-m", "add config")

	opts, _, _ := runOpts(dir)
	opts.ExpectCommit = firstCommit[:8]

	result, err := Run(opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != Refused {
		t.Fatalf("Outcome = %v, want Refused", result.Outcome)
	}
	if !strings.Contains(result.Message, firstCommit) {
		t.Fatalf("refusal message should name the commit resolved from the short form (%s), got: %s", firstCommit, result.Message)
	}
}

func TestRun_CommitFlagRefusesAmbiguousPrefix(t *testing.T) {
	dir := t.TempDir()
	runGitT(t, dir, "init", "-q")
	makeAmbiguousCommits(t, dir)

	opts, _, _ := runOpts(dir)
	opts.ExpectCommit = ambiguousPrefix

	result, err := Run(opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != Refused {
		t.Fatalf("Outcome = %v, want Refused", result.Outcome)
	}
	if !strings.Contains(result.Message, "ambiguous") {
		t.Fatalf("refusal message should mention the ambiguity, got: %s", result.Message)
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
