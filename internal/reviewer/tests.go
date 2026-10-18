package reviewer

import (
	"fmt"
	"regexp"
	"strings"
)

// TestChangeStatus names what the change did to one test file. It is read
// from the diff by inspector, never proposed by the model.
type TestChangeStatus string

const (
	TestFileAdded    TestChangeStatus = "added"
	TestFileDeleted  TestChangeStatus = "deleted"
	TestFileModified TestChangeStatus = "modified"
)

// TestJudgment is the model's judgment of one changed test file: does the
// change keep the exam that test was imposing, or weaken it?
type TestJudgment string

const (
	// ExamWeakened means the change removes, loosens, skips, or narrows
	// what the test was proving - the shape of making a failing check stop
	// failing rather than making the code right (SPEC.md section 8).
	ExamWeakened TestJudgment = "weakens_the_exam"
	// ExamIntact means the test still proves at least what it proved
	// before, or is new coverage that proves something additional.
	ExamIntact TestJudgment = "keeps_the_exam_intact"
	// ExamUnclear means the change cannot be judged from the diff alone.
	// It exists so the model has an honest answer available instead of
	// being forced into a binary it cannot support; it never reads as
	// intact.
	ExamUnclear TestJudgment = "cannot_tell"
)

// TestChange is one changed test file: the diff-derived facts about it, plus
// whatever judgment the model recorded for it.
type TestChange struct {
	Path         string           `json:"path"`
	Status       TestChangeStatus `json:"status"`
	LinesAdded   int              `json:"linesAdded"`
	LinesRemoved int              `json:"linesRemoved"`
	// Judgment is empty until the model judges this change.
	Judgment TestJudgment `json:"judgment,omitempty"`
	Reason   string       `json:"reason,omitempty"`
}

// Judged reports whether the model recorded any judgment for this change.
func (c TestChange) Judged() bool { return c.Judgment != "" }

// TestChangeVerdict is the run's single derived answer about the change's
// test files. Inspector derives it from the record; no model produces it.
type TestChangeVerdict string

const (
	// TestJudgmentNotRequested means the caller named no --test-paths
	// patterns, so inspector was never told which files are tests. This is
	// deliberately not spelled the same as NoTestChanges: "there were no
	// test changes" and "nobody said what a test is" are different facts.
	TestJudgmentNotRequested TestChangeVerdict = "test_change_judgment_not_requested"
	// NoTestChanges means patterns were given and the change touched no
	// file matching them.
	NoTestChanges TestChangeVerdict = "no_test_changes"
	// TestChangesIntact means every changed test was judged to keep its
	// exam intact.
	TestChangesIntact TestChangeVerdict = "test_changes_keep_the_exam_intact"
	// TestChangesWeaken means at least one changed test was judged to
	// weaken its exam. It outranks every other outcome: a weakened exam is
	// the thing this judgment exists to surface.
	TestChangesWeaken TestChangeVerdict = "test_changes_weaken_the_exam"
	// TestChangesUnjudged means at least one changed test carries no
	// judgment or an explicit cannot_tell, and none was judged weakened.
	// An unjudged test change never reads as intact.
	TestChangesUnjudged TestChangeVerdict = "test_changes_unjudged"
)

// ChangedTests selects the files in a parsed change that match one of the
// caller's test-path patterns, in diff order, and records what the diff
// itself proves about each one. An unusable pattern is an error rather than
// a pattern that quietly matches nothing.
func ChangedTests(files []File, patterns []string) ([]TestChange, error) {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, pattern := range patterns {
		re, err := compileGlob(pattern)
		if err != nil {
			return nil, err
		}
		compiled = append(compiled, re)
	}
	var changes []TestChange
	for _, file := range files {
		if !matchesAny(compiled, file.Path) {
			continue
		}
		change := TestChange{Path: file.Path, Status: statusOf(file)}
		for _, line := range file.Lines {
			switch line.Kind {
			case Added:
				change.LinesAdded++
			case Removed:
				change.LinesRemoved++
			}
		}
		changes = append(changes, change)
	}
	return changes, nil
}

func statusOf(file File) TestChangeStatus {
	switch {
	case file.Deleted:
		return TestFileDeleted
	case file.Added:
		return TestFileAdded
	default:
		return TestFileModified
	}
}

func matchesAny(patterns []*regexp.Regexp, path string) bool {
	for _, re := range patterns {
		if re.MatchString(path) {
			return true
		}
	}
	return false
}

// compileGlob translates one test-path pattern into a matcher with
// inspector-gate's semantics rather than Go's: the gate matches protected
// paths with bash's `[[ == ]]`, where `*` matches `/` as well, so
// "internal/*_test.go" covers every depth. One glob dialect across both
// halves is worth more here than reusing path.Match, which would silently
// stop at a directory boundary.
//
// Bracket expressions and backslash escapes are refused by name. Both mean
// something in bash that this translation does not reproduce, and a pattern
// that matches differently here than at the gate is worse than one the
// caller is told to rewrite.
func compileGlob(pattern string) (*regexp.Regexp, error) {
	if strings.TrimSpace(pattern) == "" {
		return nil, fmt.Errorf("a test-path pattern is empty")
	}
	if strings.ContainsAny(pattern, `[]\`) {
		return nil, fmt.Errorf("test-path pattern %q uses a bracket expression or backslash, which inspector review does not support because it cannot reproduce inspector-gate's matching of them; write the pattern with * and ? instead", pattern)
	}
	var b strings.Builder
	b.WriteString(`(?s)\A`)
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(`.*`)
		case '?':
			b.WriteString(`.`)
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString(`\z`)
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, fmt.Errorf("test-path pattern %q could not be compiled: %w", pattern, err)
	}
	return re, nil
}

// DeriveTestChangeVerdict reduces the recorded test changes to the run's one
// verdict. Safest reading first: a weakened exam outranks everything, and
// anything unjudged outranks intact.
func DeriveTestChangeVerdict(requested bool, changes []TestChange) TestChangeVerdict {
	if !requested {
		return TestJudgmentNotRequested
	}
	if len(changes) == 0 {
		return NoTestChanges
	}
	unjudged := false
	for _, change := range changes {
		switch change.Judgment {
		case ExamWeakened:
			return TestChangesWeaken
		case ExamIntact:
		default:
			unjudged = true
		}
	}
	if unjudged {
		return TestChangesUnjudged
	}
	return TestChangesIntact
}

// RenderTestChanges formats the derived test-change list for the model, so
// which files count as tests is inspector's finding and not open to debate.
func RenderTestChanges(changes []TestChange) string {
	var b strings.Builder
	b.WriteString("CHANGED TESTS (inspector derived this list itself from the change and the caller's test-path patterns; call judge_test_change exactly once for each path below before finishing):\n\n")
	for _, change := range changes {
		fmt.Fprintf(&b, "  %s  %s  +%d/-%d\n", change.Path, change.Status, change.LinesAdded, change.LinesRemoved)
	}
	b.WriteString("\n")
	return b.String()
}
