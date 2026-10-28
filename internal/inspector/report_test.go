package inspector

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestWriteReport(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	r := Report{
		Commit:       "abc123def4567890",
		Repo:         dir,
		CheckCommand: "make check",
		Outcome:      Red,
		StartedAt:    now,
		FinishedAt:   now.Add(time.Second),
		DurationMS:   1000,
		ExitCode:     1,
		Output:       "test failed",
	}

	path, err := WriteReport(dir, r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading report: %v", err)
	}
	var got Report
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshaling report: %v", err)
	}
	if got.Commit != r.Commit || got.Outcome != Red || got.ExitCode != 1 {
		t.Fatalf("report round-trip mismatch: %+v", got)
	}

	latest, err := os.ReadFile(filepath.Join(dir, RunsDirName, LatestReportName))
	if err != nil {
		t.Fatalf("reading latest report: %v", err)
	}
	if string(latest) != string(data) {
		t.Fatal("latest.json does not match the timestamped report")
	}

	ignore, err := os.ReadFile(filepath.Join(dir, RunsDirName, ".gitignore"))
	if err != nil {
		t.Fatalf("reading %s/.gitignore: %v", RunsDirName, err)
	}
	if string(ignore) != "*\n" {
		t.Fatalf("%s/.gitignore = %q, want %q", RunsDirName, string(ignore), "*\n")
	}
}

// Issue #12: a report that holds only part of the output has to say so, in
// the record as well as in the output itself.
func TestWriteReportRecordsTruncation(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	path, err := WriteReport(dir, Report{
		Commit:             "abc123def4567890",
		Repo:               dir,
		CheckCommand:       "script/check",
		Outcome:            Red,
		StartedAt:          now,
		FinishedAt:         now.Add(time.Second),
		ExitCode:           1,
		Output:             "head\n[inspector: 900 bytes ... dropped here ...]\ntail",
		OutputTruncated:    true,
		OutputDroppedBytes: 900,
		OutputTotalBytes:   1000,
	})
	if err != nil {
		t.Fatalf("writing the report: %v", err)
	}

	for _, file := range []string{path, filepath.Join(dir, RunsDirName, LatestReportName)} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		var got Report
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshaling %s: %v", file, err)
		}
		if !got.OutputTruncated || got.OutputDroppedBytes != 900 || got.OutputTotalBytes != 1000 {
			t.Fatalf("%s does not record the truncation: %+v", file, got)
		}
	}

	// An ordinary run says nothing at all, rather than saying zero.
	untruncated, err := WriteReport(dir, Report{Commit: "abc123def4567890", Repo: dir, FinishedAt: now.Add(2 * time.Second), Output: "all of it"})
	if err != nil {
		t.Fatalf("writing the untruncated report: %v", err)
	}
	data, err := os.ReadFile(untruncated)
	if err != nil {
		t.Fatalf("reading %s: %v", untruncated, err)
	}
	if strings.Contains(string(data), "outputTruncated") || strings.Contains(string(data), "outputDroppedBytes") {
		t.Fatalf("an untruncated run's report talks about truncation:\n%s", data)
	}
}

// Issue #12: .inspector/runs/ stops growing without limit.
func TestWriteReportPrunesOldRuns(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-time.Hour)
	var written []string
	for i := 0; i < MaxRunsKept+5; i++ {
		path, err := WriteReport(dir, Report{
			Commit:     fmt.Sprintf("%012d0000", i),
			Repo:       dir,
			Outcome:    Green,
			FinishedAt: base.Add(time.Duration(i) * time.Second),
		})
		if err != nil {
			t.Fatalf("writing report %d: %v", i, err)
		}
		written = append(written, path)
	}

	runs, err := os.ReadDir(filepath.Join(dir, RunsDirName, "runs"))
	if err != nil {
		t.Fatalf("reading the runs directory: %v", err)
	}
	if len(runs) != MaxRunsKept {
		names := make([]string, 0, len(runs))
		for _, entry := range runs {
			names = append(names, entry.Name())
		}
		t.Fatalf("the runs directory holds %d reports, want %d: %v", len(runs), MaxRunsKept, names)
	}
	// The newest are what a human chasing a red result needs, and the
	// very newest is the run whose status was just posted.
	for _, path := range written[5:] {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("a kept report is gone: %v", err)
		}
	}
	for _, path := range written[:5] {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("os.Stat(%s) = %v, want the oldest reports pruned", filepath.Base(path), err)
		}
	}
}

// Many runs inside one second are the hard case, because pruning frees a
// name for writeNewFile to reuse: the twenty-second run of a second gets
// the unsuffixed name back, which *looks* like the first run of that
// second. The bound still has to hold and the run just written - the one
// whose status is about to be posted - must still be there.
func TestWriteReportPrunesSameSecondRunsWithoutLosingTheNewest(t *testing.T) {
	dir := t.TempDir()
	finished := time.Now()
	runsDir := filepath.Join(dir, RunsDirName, "runs")

	for i := 0; i < MaxRunsKept+5; i++ {
		path, err := WriteReport(dir, Report{
			Commit:     "abc123def4567890",
			Repo:       dir,
			Outcome:    Green,
			FinishedAt: finished,
		})
		if err != nil {
			t.Fatalf("writing report %d: %v", i, err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("run %d pruned the report it had just written - the commit status about to be posted would point at nothing: %v", i, err)
		}
		runs, err := os.ReadDir(runsDir)
		if err != nil {
			t.Fatalf("reading the runs directory: %v", err)
		}
		if len(runs) > MaxRunsKept {
			t.Fatalf("after %d runs in one second the directory holds %d reports, want no more than %d", i+1, len(runs), MaxRunsKept)
		}
	}
}

// A prune that cannot delete has to name itself as that, because the
// caller's one job with it is to keep the result it already has rather
// than call the report lost.
func TestPruneRunsReportsWhatItCouldNotRemove(t *testing.T) {
	runsDir := t.TempDir()
	for i := 0; i < MaxRunsKept+2; i++ {
		name := fmt.Sprintf("%d-abc123def456.json", time.Now().Unix()-int64(i))
		if err := os.WriteFile(filepath.Join(runsDir, name), []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
	}
	if err := os.Chmod(runsDir, 0o555); err != nil {
		t.Fatalf("making the runs directory read-only: %v", err)
	}
	t.Cleanup(func() { os.Chmod(runsDir, 0o755) })

	if os.Geteuid() == 0 {
		t.Skip("running as root, permission bits are not enforced")
	}

	err := pruneRuns(runsDir, MaxRunsKept, "")
	if !errors.Is(err, ErrRunsNotPruned) {
		t.Fatalf("pruneRuns() = %v, want it to wrap ErrRunsNotPruned", err)
	}
}

// A gitignored directory is also somewhere a person may keep something of
// their own. Pruning removes only the reports inspector wrote.
func TestWriteReportPruningLeavesForeignFilesAlone(t *testing.T) {
	dir := t.TempDir()
	runsDir := filepath.Join(dir, RunsDirName, "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		t.Fatalf("creating the runs directory: %v", err)
	}
	foreign := map[string]string{
		"keep-this.json":     "{}\n",
		"notes.txt":          "mine\n",
		"not-a-stamp.json":   "{}\n",
		"1730000000.json":    "{}\n",
		"-1730000000.json":   "{}\n",
		"1730000000-.json":   "{}\n",
		"1730000000-a.jsonx": "{}\n",
	}
	for name, body := range foreign {
		if err := os.WriteFile(filepath.Join(runsDir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
	}

	base := time.Now().Add(-time.Hour)
	for i := 0; i < MaxRunsKept+5; i++ {
		if _, err := WriteReport(dir, Report{
			Commit:     fmt.Sprintf("%012d0000", i),
			Repo:       dir,
			Outcome:    Green,
			FinishedAt: base.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("writing report %d: %v", i, err)
		}
	}

	for name := range foreign {
		if _, err := os.Stat(filepath.Join(runsDir, name)); err != nil {
			t.Fatalf("pruning removed %s, which inspector did not write: %v", name, err)
		}
	}
}

// Issue #55: the per-file reasons behind the inspector-tests status have to
// reach the report, on a pass as much as on a failure - the status
// description cannot hold them, and it is the report Fabrica copies onto the
// task record.
func TestRecordTestReview(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	path, err := WriteReport(dir, Report{
		Commit:       "abc123def4567890",
		Repo:         dir,
		CheckCommand: "script/check",
		Outcome:      Green,
		StartedAt:    now,
		FinishedAt:   now.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("writing the report: %v", err)
	}

	review := TestReview{
		Vendor:  "anthropic (default, no flag given)",
		Model:   "claude-opus-5",
		State:   StatusSuccess,
		Verdict: "test_changes_keep_the_exam_intact",
		Summary: "every changed test keeps its exam intact (feature/rounding_test.go)",
		Files: []TestReviewFile{{
			Path:     "feature/rounding_test.go",
			Status:   "added",
			Judgment: "keeps_the_exam_intact",
			Reason:   "new coverage for rounding; it removes nothing the request asked for",
		}},
	}
	if err := RecordTestReview(dir, path, review); err != nil {
		t.Fatalf("recording the test review: %v", err)
	}
	// The pull request URL is patched in by a separate call afterward; both
	// patches have to survive each other, since each rewrites the whole file.
	if err := RecordPullRequestURL(dir, path, "https://github.com/inactdev/inspector/pull/9"); err != nil {
		t.Fatalf("recording the pull request URL: %v", err)
	}

	for _, file := range []string{path, filepath.Join(dir, RunsDirName, LatestReportName)} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		var got Report
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshaling %s: %v", file, err)
		}
		if got.TestReview == nil {
			t.Fatalf("%s carries no testReview at all", file)
		}
		if !reflect.DeepEqual(*got.TestReview, review) {
			t.Fatalf("%s testReview = %+v, want %+v", file, *got.TestReview, review)
		}
		if got.PullRequestURL == "" {
			t.Fatalf("%s lost its pull request URL to the test-review patch", file)
		}
		if got.Commit != "abc123def4567890" || got.CheckCommand != "script/check" || got.Outcome != Green {
			t.Fatalf("%s lost report fields to the patch: %+v", file, got)
		}
		if !strings.Contains(string(data), `"reason"`) {
			t.Fatalf("%s records no per-file reason:\n%s", file, data)
		}
	}
}

// A run whose report could not be saved has nothing to patch, and that must
// not become an error of its own: the report is notes, never authority.
func TestRecordTestReviewWithoutAReportIsANoOp(t *testing.T) {
	if err := RecordTestReview(t.TempDir(), "", TestReview{State: StatusFailure}); err != nil {
		t.Fatalf("recording a review with no report: %v", err)
	}
}

func TestWriteReportKeepsExistingGitignore(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, RunsDirName), 0o755); err != nil {
		t.Fatalf("creating %s: %v", RunsDirName, err)
	}
	path := filepath.Join(dir, RunsDirName, ".gitignore")
	if err := os.WriteFile(path, []byte("runs/\n"), 0o644); err != nil {
		t.Fatalf("seeding .gitignore: %v", err)
	}

	if _, err := WriteReport(dir, Report{Commit: "abc123def4567890", Repo: dir}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading .gitignore: %v", err)
	}
	if string(got) != "runs/\n" {
		t.Fatalf("existing .gitignore was overwritten: %q", string(got))
	}
}
