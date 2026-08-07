package inspector

import (
	"encoding/json"
	"os"
	"path/filepath"
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
