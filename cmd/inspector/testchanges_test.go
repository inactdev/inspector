package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
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
	// Issue #55: every outcome carries its own record, so the report can
	// never say less than the status did.
	if tests.Review.State != inspector.StatusFailure || tests.Review.Summary != tests.Summary {
		t.Fatalf("review = %+v, want it to record the failure and its reason", tests.Review)
	}
}

// Issue #55's first ask: a PASS records every per-file judgment and its
// reason, with the vendor and model that judged it. A pass that records
// nothing is unauditable - a model that waves every diff through reads
// exactly like a careful one.
func TestJudgeChangedTests_PassRecordsEveryPerFileReasonAndWhoJudged(t *testing.T) {
	dir, commit := newRepoWithRecordedBase(t,
		map[string]string{"feature/feature_test.go": featureTest, "feature/feature.go": "package feature\n\nfunc Discount(n int) int { return n * 9 / 10 }\n"},
		func(dir string) {
			writeFileT(t, dir, "feature/feature_test.go", featureTest+"\nfunc TestDiscountIsStable(t *testing.T) {}\n")
			writeFileT(t, dir, "feature/rounding_test.go", "package feature\n\nimport \"testing\"\n\nfunc TestRoundingIsStable(t *testing.T) {}\n")
		})
	const (
		modifiedReason = "the 10% discount assertion is untouched; the change only adds a second case"
		addedReason    = "new coverage for rounding; it removes nothing the request asked for"
	)
	stubTestJudge(t, reviewer.Result{
		TestChangeVerdict: reviewer.TestChangesIntact,
		TestChanges: []reviewer.TestChange{
			{Path: "feature/feature_test.go", Status: reviewer.TestFileModified, Judgment: reviewer.ExamIntact, Reason: modifiedReason},
			{Path: "feature/rounding_test.go", Status: reviewer.TestFileAdded, Judgment: reviewer.ExamIntact, Reason: addedReason},
		},
	}, nil)

	tests := judgeChangedTests(dir, commit, "main", []string{"*_test.go"}, testJudgmentOptions{
		Vendor: "anthropic", Model: "claude-opus-5", VendorDescription: "anthropic (default, no flag given)",
	})
	if tests.State != inspector.StatusSuccess {
		t.Fatalf("state = %q, want success", tests.State)
	}
	review := tests.Review
	if review.Vendor != "anthropic (default, no flag given)" || review.Model != "claude-opus-5" {
		t.Fatalf("review names vendor %q model %q, want the ones that judged", review.Vendor, review.Model)
	}
	if review.State != inspector.StatusSuccess || review.Verdict != string(reviewer.TestChangesIntact) {
		t.Fatalf("review state = %q verdict = %q, want the passing pair", review.State, review.Verdict)
	}
	want := []inspector.TestReviewFile{
		{Path: "feature/feature_test.go", Status: "modified", Judgment: string(reviewer.ExamIntact), Reason: modifiedReason},
		{Path: "feature/rounding_test.go", Status: "added", Judgment: string(reviewer.ExamIntact), Reason: addedReason},
	}
	if !reflect.DeepEqual(review.Files, want) {
		t.Fatalf("review files = %+v, want %+v", review.Files, want)
	}
}

// A file nobody judged is still listed, carrying no judgment: "the reviewer
// never reached this one" is a fact about the review, and leaving it out
// would read as there having been nothing to judge.
func TestJudgeChangedTests_RecordsUnreachedFilesAsUnjudged(t *testing.T) {
	dir, commit := newRepoWithRecordedBase(t, map[string]string{"feature_test.go": featureTest}, func(dir string) {
		if err := os.Remove(filepath.Join(dir, "feature_test.go")); err != nil {
			t.Fatalf("deleting the test: %v", err)
		}
	})
	stubTestJudge(t, reviewer.Result{}, errors.New("ANTHROPIC_API_KEY is not set"))

	tests := judgeChangedTests(dir, commit, "main", []string{"*_test.go"}, testJudgmentOptions{
		Model: "claude-opus-5", VendorDescription: "anthropic (default, no flag given)",
	})
	assertTestFailure(t, tests, "could not be completed")
	want := []inspector.TestReviewFile{{Path: "feature_test.go", Status: "deleted"}}
	if !reflect.DeepEqual(tests.Review.Files, want) {
		t.Fatalf("review files = %+v, want %+v", tests.Review.Files, want)
	}
	if tests.Review.Verdict != string(reviewer.TestChangesUnjudged) {
		t.Fatalf("review verdict = %q, want %q", tests.Review.Verdict, reviewer.TestChangesUnjudged)
	}
}

// Issue #55's second ask: the same list goes to stdout, so a caller that
// never opens the report file still captures what the reviewer concluded.
func TestPrintTestJudgment_PrintsEveryPerFileReason(t *testing.T) {
	const reason = "the 10% discount assertion is untouched"
	tests := testJudgment{
		Posted:  true,
		State:   inspector.StatusSuccess,
		Summary: "every changed test keeps its exam intact (feature/feature_test.go, feature/rounding_test.go)",
		Review: inspector.TestReview{
			Vendor:  "anthropic (default, no flag given)",
			Model:   "claude-opus-5",
			State:   inspector.StatusSuccess,
			Verdict: string(reviewer.TestChangesIntact),
			Files: []inspector.TestReviewFile{
				{Path: "feature/feature_test.go", Status: "modified", Judgment: string(reviewer.ExamIntact), Reason: reason},
				{Path: "feature/rounding_test.go", Status: "added"},
			},
		},
	}

	var stdout, stderr bytes.Buffer
	printTestJudgment(&stdout, &stderr, tests)
	out := stdout.String()
	for _, want := range []string{
		inspector.TestsStatusContext + ": success",
		"judged by vendor: anthropic (default, no flag given), model: claude-opus-5",
		"feature/feature_test.go (modified): " + string(reviewer.ExamIntact) + " - " + reason,
		"feature/rounding_test.go (added): unjudged",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout = %q, want it to contain %q", out, want)
		}
	}
	if stderr.String() != "" {
		t.Fatalf("a passing judgment wrote to stderr: %q", stderr.String())
	}
}

// The judgment happens after the report is written, so it is patched in
// afterward - and a pass has to be patched in exactly as a failure does.
func TestRecordTestReview_PatchesThePassingReviewIntoTheReport(t *testing.T) {
	dir, commit := newRepoWithRecordedBase(t, map[string]string{"feature_test.go": featureTest}, func(dir string) {
		writeFileT(t, dir, "feature_test.go", featureTest+"\nfunc TestDiscountIsStable(t *testing.T) {}\n")
	})
	const reason = "the 10% discount assertion is untouched; the change only adds a case"
	stubTestJudge(t, reviewer.Result{
		TestChangeVerdict: reviewer.TestChangesIntact,
		TestChanges:       []reviewer.TestChange{{Path: "feature_test.go", Status: reviewer.TestFileModified, Judgment: reviewer.ExamIntact, Reason: reason}},
	}, nil)
	tests := judgeChangedTests(dir, commit, "main", []string{"*_test.go"}, testJudgmentOptions{
		Model: "claude-opus-5", VendorDescription: "anthropic (default, no flag given)",
	})

	reportPath, err := inspector.WriteReport(dir, inspector.Report{Commit: commit, Repo: dir, Outcome: inspector.Green})
	if err != nil {
		t.Fatalf("writing the report: %v", err)
	}
	var stderr bytes.Buffer
	recordTestReview(&stderr, dir, reportPath, tests)
	if stderr.String() != "" {
		t.Fatalf("recording the review warned: %q", stderr.String())
	}

	assertReportTestReview(t, filepath.Join(dir, inspector.RunsDirName, inspector.LatestReportName), reason)
}

// A review that could not be written is a warning and nothing else: the
// report is notes, never authority, so it must not change the run's result.
func TestRecordTestReview_UnwritableReportOnlyWarns(t *testing.T) {
	var stderr bytes.Buffer
	recordTestReview(&stderr, t.TempDir(), filepath.Join(t.TempDir(), "absent.json"), testJudgment{
		Posted: true,
		State:  inspector.StatusSuccess,
		Review: inspector.TestReview{State: inspector.StatusSuccess},
	})
	if !strings.Contains(stderr.String(), "could not record the changed-test review") {
		t.Fatalf("stderr = %q, want a warning naming the failure", stderr.String())
	}
}

// End to end, as Fabrica runs it: a green run whose change touches a test
// leaves the reviewer's per-file reasons both on stdout and in the report
// file Fabrica copies onto the task record.
func TestCLI_GreenRecordsAndPrintsThePassingTestReview(t *testing.T) {
	requirePublicationPlatform(t)
	requireDocker(t)
	dir, _ := newRepoWithRecordedBase(t,
		map[string]string{
			".inspector.json": `{"check": "true", "testPaths": ["*_test.go"]}`,
			"feature_test.go": featureTest,
		},
		func(dir string) {
			writeFileT(t, dir, "feature_test.go", featureTest+"\nfunc TestDiscountIsStable(t *testing.T) {}\n")
		})
	stubGitHubStatusAPI(t, dir, http.StatusCreated, "", nil)
	const reason = "the 10% discount assertion is untouched; the change only adds a case"
	stubTestJudge(t, reviewer.Result{
		TestChangeVerdict: reviewer.TestChangesIntact,
		TestChanges:       []reviewer.TestChange{{Path: "feature_test.go", Status: reviewer.TestFileModified, Judgment: reviewer.ExamIntact, Reason: reason}},
	}, nil)

	code, stdout, stderr := runCLI(t, dir, "--branch", "feature", "--model", "claude-opus-5")
	if code != exitGreen {
		t.Fatalf("exit code = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitGreen, stdout, stderr)
	}
	if !strings.Contains(stdout, reason) || !strings.Contains(stdout, "feature_test.go (modified)") {
		t.Fatalf("stdout = %q, want the reviewer's own per-file reason in it", stdout)
	}
	assertReportTestReview(t, filepath.Join(dir, inspector.RunsDirName, inspector.LatestReportName), reason)
}

func assertReportTestReview(t *testing.T, path, wantReason string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var report inspector.Report
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("unmarshaling %s: %v", path, err)
	}
	if report.TestReview == nil {
		t.Fatalf("%s carries no record of the test review at all:\n%s", path, data)
	}
	review := *report.TestReview
	if review.State != inspector.StatusSuccess || review.Verdict != string(reviewer.TestChangesIntact) {
		t.Fatalf("recorded review state = %q verdict = %q, want the passing pair", review.State, review.Verdict)
	}
	if review.Model != "claude-opus-5" || review.Vendor == "" {
		t.Fatalf("recorded review names vendor %q model %q, want both", review.Vendor, review.Model)
	}
	if len(review.Files) != 1 {
		t.Fatalf("recorded review files = %+v, want exactly the one changed test", review.Files)
	}
	file := review.Files[0]
	if file.Path != "feature_test.go" || file.Status != "modified" || file.Judgment != string(reviewer.ExamIntact) {
		t.Fatalf("recorded file = %+v, want the modified test judged intact", file)
	}
	if file.Reason != wantReason {
		t.Fatalf("recorded reason = %q, want %q", file.Reason, wantReason)
	}
}
