package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inactdev/inspector/internal/inspector"
	"github.com/inactdev/inspector/internal/reviewer"
)

// stubTestJudge replaces the reviewer agent for the duration of a test. The
// returned pointer records the diff the judgment was actually handed, so a
// test can prove inspector sent the change it claims to have judged rather
// than only that it posted a status.
type recordedJudgment struct {
	Calls   int
	Options reviewer.AgentOptions
}

func stubTestJudge(t *testing.T, result reviewer.Result, err error) *recordedJudgment {
	t.Helper()
	recorded := &recordedJudgment{}
	previous := judgeTestChanges
	judgeTestChanges = func(_ context.Context, opts reviewer.AgentOptions) (reviewer.Result, error) {
		recorded.Calls++
		recorded.Options = opts
		return result, err
	}
	t.Cleanup(func() { judgeTestChanges = previous })
	return recorded
}

// newRepoWithRecordedBase builds a repo whose first commit is also what
// `origin/main` points at - the base the judged commit is diffed against -
// and then applies changes as a second commit. The returned commit is the one
// to publish.
func newRepoWithRecordedBase(t *testing.T, base map[string]string, changes func(dir string)) (dir, commit string) {
	t.Helper()
	dir = newTestRepo(t, base)
	baseCommit := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	runGitT(t, dir, "update-ref", "refs/remotes/origin/main", baseCommit)
	runGitT(t, dir, "checkout", "-q", "-b", "local-checkout")
	changes(dir)
	runGitT(t, dir, "add", "-A")
	runGitT(t, dir, "commit", "-q", "-m", "change")
	return dir, strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
}

func writeFileT(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", name, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}

const featureTest = `package feature

import "testing"

func TestDiscountApplies(t *testing.T) {
	if Discount(100) != 90 {
		t.Fatal("the requested 10% discount was not applied")
	}
}
`

// Issue #44's first Done-when case: a pull request that deletes a test
// encoding the requested feature is red, with the reason.
func TestPublishGreen_DeletedTestEncodingTheFeatureIsRedWithTheReason(t *testing.T) {
	requirePublicationPlatform(t)
	dir, commit := newRepoWithRecordedBase(t,
		map[string]string{"feature/feature_test.go": featureTest, "feature/feature.go": "package feature\n\nfunc Discount(n int) int { return n }\n"},
		func(dir string) {
			if err := os.Remove(filepath.Join(dir, "feature/feature_test.go")); err != nil {
				t.Fatalf("deleting the test: %v", err)
			}
		})
	stub := stubGitHubStatusAPI(t, dir, http.StatusCreated, "", nil)
	const reason = "the deleted test was the only one proving the requested 10% discount"
	judge := stubTestJudge(t, reviewer.Result{
		TestChangeVerdict: reviewer.TestChangesWeaken,
		TestChanges: []reviewer.TestChange{{
			Path:     "feature/feature_test.go",
			Status:   reviewer.TestFileDeleted,
			Judgment: reviewer.ExamWeakened,
			Reason:   reason,
		}},
	}, nil)

	_, tests, err := publishGreen(
		capturePublicationTarget(dir), publicationBranchTarget{Branch: "feature"},
		inspector.Result{Outcome: inspector.Green, Commit: commit, TestPaths: []string{"*_test.go"}},
		testJudgmentOptions{Vendor: "anthropic", Model: "claude-opus-5", VendorDescription: "anthropic (default, no flag given)"},
	)
	if err != nil {
		t.Fatalf("publishGreen: %v", err)
	}
	if judge.Calls != 1 {
		t.Fatalf("judgment calls = %d, want 1", judge.Calls)
	}
	if !strings.Contains(string(judge.Options.Diff), "feature/feature_test.go") || !strings.Contains(string(judge.Options.Diff), "TestDiscountApplies") {
		t.Fatalf("the judged diff did not carry the deleted test:\n%s", judge.Options.Diff)
	}
	if judge.Options.Model != "claude-opus-5" || judge.Options.Vendor != "anthropic" {
		t.Fatalf("judged with vendor %q model %q, want the flags given to inspector run", judge.Options.Vendor, judge.Options.Model)
	}

	posted, ok := stub.statusFor(inspector.TestsStatusContext)
	if !ok {
		t.Fatalf("no %q status was posted; statuses = %+v", inspector.TestsStatusContext, stub.Requests)
	}
	if posted.State != string(inspector.StatusFailure) {
		t.Fatalf("%q state = %q, want failure", inspector.TestsStatusContext, posted.State)
	}
	if !strings.Contains(posted.Desc, "weakens the exam") || !strings.Contains(posted.Desc, "feature/feature_test.go") {
		t.Fatalf("%q description = %q, want it to name the weakened test and say so", inspector.TestsStatusContext, posted.Desc)
	}
	if !strings.Contains(posted.Desc, "anthropic") {
		t.Fatalf("%q description = %q, want the vendor named", inspector.TestsStatusContext, posted.Desc)
	}
	if !strings.Contains(tests.Summary, reason) {
		t.Fatalf("summary = %q, want the model's own reason", tests.Summary)
	}
	if tests.Warning == "" {
		t.Fatal("a failing test judgment printed no loud warning for the worker")
	}

	// The green check status still had to be recorded, and the branch still
	// had to move: without them the gate never gets to read this failure.
	check, ok := stub.statusFor(inspector.StatusContext)
	if !ok || check.State != string(inspector.StatusSuccess) {
		t.Fatalf("%q status = %+v (posted %t), want success", inspector.StatusContext, check, ok)
	}
	if got, exists := remoteRef(t, stub.Remote, "refs/heads/feature"); !exists || got != commit {
		t.Fatalf("published branch = (%q, %t), want (%q, true)", got, exists, commit)
	}
}

// Issue #44's second Done-when case: a pull request that adds an unrelated
// test is green - including the inspector-tests status the gate needs in order
// to let its changed test file through without the Client's override.
func TestPublishGreen_AddedUnrelatedTestClearsTheGateWithoutAnOverride(t *testing.T) {
	requirePublicationPlatform(t)
	dir, commit := newRepoWithRecordedBase(t,
		map[string]string{"feature/feature.go": "package feature\n\nfunc Discount(n int) int { return n }\n"},
		func(dir string) {
			writeFileT(t, dir, "feature/rounding_test.go", "package feature\n\nimport \"testing\"\n\nfunc TestRoundingIsStable(t *testing.T) {}\n")
		})
	stub := stubGitHubStatusAPI(t, dir, http.StatusCreated, "", nil)
	judge := stubTestJudge(t, reviewer.Result{
		TestChangeVerdict: reviewer.TestChangesIntact,
		TestChanges: []reviewer.TestChange{{
			Path:     "feature/rounding_test.go",
			Status:   reviewer.TestFileAdded,
			Judgment: reviewer.ExamIntact,
			Reason:   "a new test about rounding; it removes nothing the request asked for",
		}},
	}, nil)

	_, tests, err := publishGreen(
		capturePublicationTarget(dir), publicationBranchTarget{Branch: "feature"},
		inspector.Result{Outcome: inspector.Green, Commit: commit, TestPaths: []string{"*_test.go"}},
		testJudgmentOptions{Vendor: "anthropic", Model: "claude-opus-5", VendorDescription: "anthropic (default, no flag given)"},
	)
	if err != nil {
		t.Fatalf("publishGreen: %v", err)
	}
	if judge.Calls != 1 {
		t.Fatalf("judgment calls = %d, want 1", judge.Calls)
	}
	posted, ok := stub.statusFor(inspector.TestsStatusContext)
	if !ok {
		t.Fatalf("no %q status was posted; statuses = %+v", inspector.TestsStatusContext, stub.Requests)
	}
	if posted.State != string(inspector.StatusSuccess) {
		t.Fatalf("%q state = %q, want success - the gate clears a changed test on nothing else", inspector.TestsStatusContext, posted.State)
	}
	if tests.Warning != "" {
		t.Fatalf("an intact-test judgment printed a warning: %q", tests.Warning)
	}
	check, ok := stub.statusFor(inspector.StatusContext)
	if !ok || check.State != string(inspector.StatusSuccess) {
		t.Fatalf("%q status = %+v (posted %t), want success", inspector.StatusContext, check, ok)
	}
	if got, exists := remoteRef(t, stub.Remote, "refs/heads/feature"); !exists || got != commit {
		t.Fatalf("published branch = (%q, %t), want (%q, true)", got, exists, commit)
	}
}

// The status is owed only when a test file actually changed, so a commit that
// touches none of them must leave the gate exactly as it was before this
// status existed.
func TestPublishGreen_NoChangedTestPostsNoTestStatus(t *testing.T) {
	requirePublicationPlatform(t)
	dir, commit := newRepoWithRecordedBase(t,
		map[string]string{"feature/feature.go": "package feature\n\nfunc Discount(n int) int { return n }\n"},
		func(dir string) {
			writeFileT(t, dir, "feature/feature.go", "package feature\n\nfunc Discount(n int) int { return n * 9 / 10 }\n")
		})
	stub := stubGitHubStatusAPI(t, dir, http.StatusCreated, "", nil)
	judge := stubTestJudge(t, reviewer.Result{TestChangeVerdict: reviewer.TestChangesIntact}, nil)

	_, tests, err := publishGreen(
		capturePublicationTarget(dir), publicationBranchTarget{Branch: "feature"},
		inspector.Result{Outcome: inspector.Green, Commit: commit, TestPaths: []string{"*_test.go"}},
		testJudgmentOptions{Model: "claude-opus-5", VendorDescription: "anthropic (default, no flag given)"},
	)
	if err != nil {
		t.Fatalf("publishGreen: %v", err)
	}
	if judge.Calls != 0 {
		t.Fatalf("judgment calls = %d, want 0 - no test file changed", judge.Calls)
	}
	if tests.Posted {
		t.Fatalf("a change touching no test file posted %q as %q", inspector.TestsStatusContext, tests.State)
	}
	if _, ok := stub.statusFor(inspector.TestsStatusContext); ok {
		t.Fatalf("statuses = %+v, want no %q among them", stub.Requests, inspector.TestsStatusContext)
	}
}

// A project that never declared testPaths gets no judgment and no status -
// never a success one, which would hand it a status clearing protected test
// files it never asked to have judged.
func TestPublishGreen_NoTestPathsPostsNoTestStatus(t *testing.T) {
	requirePublicationPlatform(t)
	dir, commit := newRepoWithRecordedBase(t,
		map[string]string{"feature/feature_test.go": featureTest},
		func(dir string) {
			if err := os.Remove(filepath.Join(dir, "feature/feature_test.go")); err != nil {
				t.Fatalf("deleting the test: %v", err)
			}
		})
	stub := stubGitHubStatusAPI(t, dir, http.StatusCreated, "", nil)
	judge := stubTestJudge(t, reviewer.Result{TestChangeVerdict: reviewer.TestChangesIntact}, nil)

	_, tests, err := publishGreen(
		capturePublicationTarget(dir), publicationBranchTarget{Branch: "feature"},
		inspector.Result{Outcome: inspector.Green, Commit: commit},
		testJudgmentOptions{Model: "claude-opus-5"},
	)
	if err != nil {
		t.Fatalf("publishGreen: %v", err)
	}
	if judge.Calls != 0 {
		t.Fatalf("judgment calls = %d, want 0 - the project declared no testPaths", judge.Calls)
	}
	if tests.Posted {
		t.Fatalf("a project with no testPaths posted %q as %q", inspector.TestsStatusContext, tests.State)
	}
	if _, ok := stub.statusFor(inspector.TestsStatusContext); ok {
		t.Fatalf("statuses = %+v, want no %q among them", stub.Requests, inspector.TestsStatusContext)
	}
}

func TestJudgeChangedTests_FailsClosed(t *testing.T) {
	deleteTheTest := func(dir string) {
		if err := os.Remove(filepath.Join(dir, "feature_test.go")); err != nil {
			t.Fatalf("deleting the test: %v", err)
		}
	}

	t.Run("no model to judge with", func(t *testing.T) {
		dir, commit := newRepoWithRecordedBase(t, map[string]string{"feature_test.go": featureTest}, deleteTheTest)
		judge := stubTestJudge(t, reviewer.Result{TestChangeVerdict: reviewer.TestChangesIntact}, nil)

		tests := judgeChangedTests(dir, commit, "main", []string{"*_test.go"}, testJudgmentOptions{})
		if judge.Calls != 0 {
			t.Fatalf("judgment calls = %d, want 0 with no model named", judge.Calls)
		}
		assertTestFailure(t, tests, "no --model was given")
	})

	t.Run("the model could not be reached", func(t *testing.T) {
		dir, commit := newRepoWithRecordedBase(t, map[string]string{"feature_test.go": featureTest}, deleteTheTest)
		stubTestJudge(t, reviewer.Result{}, errors.New("ANTHROPIC_API_KEY is not set"))

		tests := judgeChangedTests(dir, commit, "main", []string{"*_test.go"}, testJudgmentOptions{Model: "claude-opus-5"})
		assertTestFailure(t, tests, "could not be completed")
	})

	t.Run("the model left a change unjudged", func(t *testing.T) {
		dir, commit := newRepoWithRecordedBase(t, map[string]string{"feature_test.go": featureTest}, deleteTheTest)
		stubTestJudge(t, reviewer.Result{
			TestChangeVerdict: reviewer.TestChangesUnjudged,
			TestChanges:       []reviewer.TestChange{{Path: "feature_test.go", Status: reviewer.TestFileDeleted}},
		}, nil)

		tests := judgeChangedTests(dir, commit, "main", []string{"*_test.go"}, testJudgmentOptions{Model: "claude-opus-5"})
		assertTestFailure(t, tests, "carries no judgment")
	})

	t.Run("the base branch is not known locally", func(t *testing.T) {
		dir := newTestRepo(t, map[string]string{"feature_test.go": featureTest})
		commit := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))

		tests := judgeChangedTests(dir, commit, "main", []string{"*_test.go"}, testJudgmentOptions{Model: "claude-opus-5"})
		assertTestFailure(t, tests, "could not be read")
	})

	t.Run("the project's patterns cannot be used", func(t *testing.T) {
		dir, commit := newRepoWithRecordedBase(t, map[string]string{"feature_test.go": featureTest}, deleteTheTest)

		tests := judgeChangedTests(dir, commit, "main", []string{"[bad"}, testJudgmentOptions{Model: "claude-opus-5"})
		assertTestFailure(t, tests, "testPaths")
	})
}

func assertTestFailure(t *testing.T, tests testJudgment, want string) {
	t.Helper()
	if !tests.Posted {
		t.Fatal("an unconfirmed test judgment posted no status at all, so the gate would see nothing")
	}
	if tests.State != inspector.StatusFailure {
		t.Fatalf("state = %q, want failure", tests.State)
	}
	if !strings.Contains(tests.Summary, want) {
		t.Fatalf("summary = %q, want it to contain %q", tests.Summary, want)
	}
	if tests.Warning == "" {
		t.Fatal("a failing test judgment printed no loud warning for the worker")
	}
}
