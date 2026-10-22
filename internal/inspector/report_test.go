package inspector

import (
	"encoding/json"
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
