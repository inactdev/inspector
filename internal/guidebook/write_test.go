package guidebook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func read(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(content)
}

func TestWrite_CreatesTheGuidebookWhenThereIsNone(t *testing.T) {
	root := t.TempDir()
	outcome, err := Write(root, []byte("first\n"))
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if outcome.Path != FileName || outcome.Draft || outcome.ReplacedDraft {
		t.Fatalf("outcome = %#v, want the real file written", outcome)
	}
	if got := read(t, filepath.Join(root, FileName)); got != "first\n" {
		t.Errorf("%s = %q", FileName, got)
	}
	if !strings.Contains(outcome.Describe(), "unconfirmed") {
		t.Errorf("Describe() = %q, want it to say the file is unconfirmed", outcome.Describe())
	}
}

// TestWrite_NeverOverwritesAConfirmedGuidebook is issue #22's "running it twice
// is safe", decided in favor of never clobbering: the file on disk may have
// been read, corrected and signed by hand, and a generator that discards that
// has taught everyone to stop confirming.
func TestWrite_NeverOverwritesAConfirmedGuidebook(t *testing.T) {
	root := t.TempDir()
	confirmed := "---\nconfirmed: 2026-10-25 Jane Mallory\n---\nhand-corrected\n"
	if err := os.WriteFile(filepath.Join(root, FileName), []byte(confirmed), 0o644); err != nil {
		t.Fatalf("seeding a confirmed guidebook: %v", err)
	}

	outcome, err := Write(root, []byte("regenerated\n"))
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if outcome.Path != DraftFileName || !outcome.Draft {
		t.Fatalf("outcome = %#v, want the new document written to the draft", outcome)
	}
	if outcome.ReplacedDraft {
		t.Error("outcome.ReplacedDraft = true, but there was no earlier draft")
	}
	if got := read(t, filepath.Join(root, FileName)); got != confirmed {
		t.Fatalf("%s = %q, want it untouched - a human's confirmation is not inspector's to discard", FileName, got)
	}
	if got := read(t, filepath.Join(root, DraftFileName)); got != "regenerated\n" {
		t.Errorf("%s = %q", DraftFileName, got)
	}
	if !strings.Contains(outcome.Describe(), "left untouched") || !strings.Contains(outcome.Describe(), DraftFileName) {
		t.Errorf("Describe() = %q, want it to say outright that there are now two files", outcome.Describe())
	}
}

// TestWrite_ReplacesAnEarlierDraftAndSaysSo - a draft is inspector's own
// output and the thing a person is expected to move into place, so a third run
// must not leave the project stuck; it does say what it replaced.
func TestWrite_ReplacesAnEarlierDraftAndSaysSo(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, FileName), []byte("real\n"), 0o644); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := Write(root, []byte("draft one\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	outcome, err := Write(root, []byte("draft two\n"))
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if !outcome.Draft || !outcome.ReplacedDraft {
		t.Fatalf("outcome = %#v, want a replaced draft reported", outcome)
	}
	if got := read(t, filepath.Join(root, DraftFileName)); got != "draft two\n" {
		t.Errorf("%s = %q, want the newest draft", DraftFileName, got)
	}
	if got := read(t, filepath.Join(root, FileName)); got != "real\n" {
		t.Errorf("%s = %q, want it still untouched after a third run", FileName, got)
	}
	if !strings.Contains(outcome.Describe(), "replaced the earlier") {
		t.Errorf("Describe() = %q, want the replacement said outright", outcome.Describe())
	}
}

// TestWrite_RunningTwiceOverAFreshProjectIsSafe is the whole of "running it
// twice is safe" in one case: the first run creates the guidebook, the second
// leaves it alone.
func TestWrite_RunningTwiceOverAFreshProjectIsSafe(t *testing.T) {
	root := t.TempDir()
	if _, err := Write(root, []byte("first\n")); err != nil {
		t.Fatalf("first Write() error = %v", err)
	}
	outcome, err := Write(root, []byte("second\n"))
	if err != nil {
		t.Fatalf("second Write() error = %v", err)
	}
	if !outcome.Draft {
		t.Fatal("the second run wrote the real file instead of a draft")
	}
	if got := read(t, filepath.Join(root, FileName)); got != "first\n" {
		t.Errorf("%s = %q, want the first run's file intact", FileName, got)
	}
}

func TestWrite_ReportsAnUnwritableRoot(t *testing.T) {
	_, err := Write(filepath.Join(t.TempDir(), "no-such-directory"), []byte("x"))
	if err == nil || !strings.Contains(err.Error(), FileName) {
		t.Fatalf("Write() error = %v, want a failure naming the file it could not write", err)
	}
}
