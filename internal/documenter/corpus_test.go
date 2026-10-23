package documenter

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadCorpus(t *testing.T, files map[string]string) *Corpus {
	t.Helper()
	corpus, err := LoadCorpus(newRepo(t, files))
	if err != nil {
		t.Fatalf("LoadCorpus() error = %v", err)
	}
	return corpus
}

func TestLoadCorpus_HoldsOnlyTrackedFiles(t *testing.T) {
	repo := newRepo(t, map[string]string{"README.md": "the default is 30 seconds\n"})
	// Written after the commit, so git does not know about it. An untracked
	// file is not a claim the project publishes, and it is where a credential
	// lives - the reason the corpus is the index and not the filesystem.
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("TOKEN=secret\n"), 0o600); err != nil {
		t.Fatalf("writing the untracked file: %v", err)
	}
	corpus, err := LoadCorpus(repo)
	if err != nil {
		t.Fatalf("LoadCorpus() error = %v", err)
	}
	if !corpus.Has("README.md") {
		t.Fatal("corpus does not hold the tracked README")
	}
	if corpus.Has(".env") {
		t.Fatal("corpus holds an untracked file")
	}
	if strings.Contains(corpus.Search("TOKEN"), "secret") {
		t.Fatal("search reached an untracked file's contents")
	}
}

func TestLoadCorpus_SkipsWhatNobodyReadsAsAClaim(t *testing.T) {
	repo := newRepo(t, map[string]string{
		"README.md": "a real claim\n",
		"logo.png":  "PNG\x00\x01binary",
		"big.txt":   strings.Repeat("x", maxFileBytes+1),
	})
	if err := os.Symlink("README.md", filepath.Join(repo, "link.md")); err != nil {
		t.Skipf("this platform cannot create a symlink: %v", err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "add the link")

	corpus, err := LoadCorpus(repo)
	if err != nil {
		t.Fatalf("LoadCorpus() error = %v", err)
	}
	for path, reason := range map[string]SkipReason{
		"logo.png": SkippedBinary,
		"big.txt":  SkippedTooLarge,
		// A tracked symlink's target can sit anywhere on the host, so the
		// corpus is the files of this tree rather than whatever they name.
		"link.md": SkippedNotRegular,
	} {
		if corpus.Has(path) {
			t.Errorf("corpus holds %s, which should have been skipped as %q", path, reason)
		}
	}
	summary := corpus.Summary()
	if summary.Files != 1 || summary.Skipped != 3 {
		t.Fatalf("summary = %#v, want one file read and three skipped", summary)
	}
	if len(summary.SkippedExamples) != 3 {
		t.Fatalf("skipped examples = %#v, want each skip named with its reason", summary.SkippedExamples)
	}
}

func TestLoadCorpus_RefusesWhenNothingReadableIsTracked(t *testing.T) {
	// Reporting "nothing was falsified" here would be a judgment over an
	// empty corpus, which is the one answer nobody can act on.
	_, err := LoadCorpus(newRepo(t, map[string]string{"blob.bin": "\x00\x01\x02"}))
	if err == nil || !strings.Contains(err.Error(), "no readable tracked text file") {
		t.Fatalf("LoadCorpus() error = %v, want a refusal naming the empty corpus", err)
	}
}

func TestLoadCorpus_NamesEverySkipItCappedTheListingOf(t *testing.T) {
	files := map[string]string{"README.md": "a real claim\n"}
	for i := 0; i < 15; i++ {
		files[fmt.Sprintf("asset%02d.bin", i)] = "\x00binary"
	}
	summary := loadCorpus(t, files).Summary()
	if summary.Skipped != 15 {
		t.Fatalf("summary.Skipped = %d, want all 15 counted even though only some are listed", summary.Skipped)
	}
	if len(summary.SkippedExamples) != 10 {
		t.Fatalf("skipped examples = %d, want the listing capped at ten with the full count kept", len(summary.SkippedExamples))
	}
}

func TestCorpusRead_AnnotatesLinesAndSaysWhereItStopped(t *testing.T) {
	var lines []string
	for i := 1; i <= maxReadLines+5; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	corpus := loadCorpus(t, map[string]string{"long.md": strings.Join(lines, "\n") + "\n"})

	first := corpus.Read("long.md", 0)
	if !strings.Contains(first, "     1  line 1") {
		t.Fatalf("read = %q, want the first line carrying its number", first)
	}
	if !strings.Contains(first, fmt.Sprintf("call read_file again with from_line %d", maxReadLines+1)) {
		t.Fatalf("read = %q, want the truncation stated with how to continue", first)
	}
	rest := corpus.Read("long.md", maxReadLines+1)
	if !strings.Contains(rest, fmt.Sprintf("%6d  line %d", maxReadLines+5, maxReadLines+5)) {
		t.Fatalf("read = %q, want the tail of the file", rest)
	}
	if got := corpus.Read("nowhere.md", 1); !strings.Contains(got, "not a file in the corpus") {
		t.Fatalf("read = %q, want a path outside the corpus refused", got)
	}
	if got := corpus.Read("long.md", maxReadLines+500); !strings.Contains(got, "nothing at line") {
		t.Fatalf("read = %q, want a line past the end named rather than returned empty", got)
	}
}

func TestCorpusSearch_NamesTheCapRatherThanLettingItReadAsTheWholeAnswer(t *testing.T) {
	var lines []string
	for i := 0; i < maxSearchMatches+10; i++ {
		lines = append(lines, "the default is 30 seconds")
	}
	corpus := loadCorpus(t, map[string]string{"notes.md": strings.Join(lines, "\n") + "\n"})

	got := corpus.Search("default")
	if !strings.Contains(got, "there are more") {
		t.Fatalf("search = %q, want the cap stated outright", got)
	}
	if strings.Count(got, "notes.md:") != maxSearchMatches {
		t.Fatalf("search returned %d matches, want the cap of %d", strings.Count(got, "notes.md:"), maxSearchMatches)
	}
	if got := corpus.Search("nothing here"); !strings.Contains(got, "no corpus file contains") {
		t.Fatalf("search = %q, want an explicit no-match answer", got)
	}
	if got := corpus.Search("  "); !strings.Contains(got, "non-empty query") {
		t.Fatalf("search = %q, want an empty query refused", got)
	}
	if got := corpus.Search("THE DEFAULT"); !strings.Contains(got, "notes.md:1") {
		t.Fatalf("search = %q, want matching to ignore case", got)
	}
}

func TestCorpusList_ListsByPrefix(t *testing.T) {
	corpus := loadCorpus(t, map[string]string{
		"README.md":        "top\n",
		"docs/install.md":  "install\n",
		"docs/upgrade.md":  "upgrade\n",
		"script/check.txt": "check\n",
	})
	got := corpus.List("docs/")
	if !strings.Contains(got, "docs/install.md") || !strings.Contains(got, "docs/upgrade.md") {
		t.Fatalf("list = %q, want both docs files", got)
	}
	if strings.Contains(got, "README.md") {
		t.Fatalf("list = %q, want only the prefixed files", got)
	}
	if got := corpus.List("nowhere/"); !strings.Contains(got, "no corpus file has the prefix") {
		t.Fatalf("list = %q, want an explicit empty answer", got)
	}
}

func TestCorpusDescribe_ListsEveryPathForASmallProject(t *testing.T) {
	corpus := loadCorpus(t, map[string]string{"README.md": "top\n", "docs/install.md": "install\n"})
	got := corpus.Describe()
	if !strings.Contains(got, "README.md") || !strings.Contains(got, "docs/install.md") {
		t.Fatalf("describe = %q, want every corpus path named", got)
	}
	if !strings.Contains(got, "2 readable text files") {
		t.Fatalf("describe = %q, want the corpus size stated", got)
	}
}

const claimFixture = `# Limits

The check container is bounded: memory is capped at 2g, the process
count at 512, and the network is denied unless the project opts in.

Nothing else is capped.
`

func TestVerifyClaim_AcceptsTheClaimWhereItActuallyIs(t *testing.T) {
	corpus := loadCorpus(t, map[string]string{"limits.md": claimFixture})
	// The claim spans lines 3 and 4; a pointer at its first line is right,
	// and one within the slack is still a pointer a reader can follow.
	for _, line := range []int{3, 4, 5} {
		if err := corpus.VerifyClaim("limits.md", line, "memory is capped at 2g, the process\ncount at 512"); err != nil {
			t.Errorf("VerifyClaim(line %d) error = %v, want it accepted", line, err)
		}
	}
	if err := corpus.VerifyClaim("limits.md", 3, "memory   is capped\n   at 2g"); err != nil {
		t.Errorf("VerifyClaim() error = %v, want re-wrapped whitespace to compare equal", err)
	}
}

func TestVerifyClaim_RejectsAPointerNobodyCanFollow(t *testing.T) {
	corpus := loadCorpus(t, map[string]string{"limits.md": claimFixture})
	err := corpus.VerifyClaim("limits.md", 1, "the process count at 512")
	if err == nil || !strings.Contains(err.Error(), "not at line 1") {
		t.Fatalf("VerifyClaim() error = %v, want the real claim at the wrong line named as such", err)
	}
}

func TestVerifyClaim_RejectsAClaimTheFileNeverMade(t *testing.T) {
	corpus := loadCorpus(t, map[string]string{"limits.md": claimFixture})
	err := corpus.VerifyClaim("limits.md", 3, "memory is capped at 8g")
	if err == nil || !strings.Contains(err.Error(), "does not contain the quoted claim") {
		t.Fatalf("VerifyClaim() error = %v, want an invented claim refused", err)
	}
}

func TestVerifyClaim_RejectsAnEmptyQuoteAndAnOutOfRangeLine(t *testing.T) {
	corpus := loadCorpus(t, map[string]string{"limits.md": claimFixture})
	if err := corpus.VerifyClaim("limits.md", 3, "   "); err == nil {
		t.Fatal("VerifyClaim() accepted an empty quoted claim")
	}
	if err := corpus.VerifyClaim("limits.md", 400, "Nothing else is capped."); err == nil {
		t.Fatal("VerifyClaim() accepted a line the file does not have")
	}
	if err := corpus.VerifyClaim("nowhere.md", 1, "anything"); err == nil {
		t.Fatal("VerifyClaim() accepted a file outside the corpus")
	}
}

// TestLoadCorpus_ReadsInspectorsOwnProject dogfoods the corpus against this
// repository, the way the rest of inspector runs against itself. It is the
// only test here that touches a real project, and it is worth one: every
// other test builds a tiny repo, so none of them would notice the corpus
// failing on a tree with submodule-shaped entries, nested directories, or a
// developer's dirty working tree.
func TestLoadCorpus_ReadsInspectorsOwnProject(t *testing.T) {
	corpus, err := LoadCorpus(".")
	if err != nil {
		t.Fatalf("LoadCorpus() error = %v", err)
	}
	for _, path := range []string{"README.md", "SPEC.md", "internal/llm/adapter.go"} {
		if !corpus.Has(path) {
			t.Errorf("corpus does not hold %s", path)
		}
	}
	// Never part of a project's tracked files, and the one file in a git
	// directory that can carry a credential.
	if corpus.Has(".git/config") {
		t.Error("corpus reached into the git directory")
	}
	if !strings.Contains(corpus.Read("SPEC.md", 1), "     1  ") {
		t.Error("a real file read back without its line annotation")
	}
}
