package documenter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newFixtureRepo copies one testdata tree into a fresh git repository and
// commits it, because the corpus is built from tracked files and nothing else.
func newFixtureRepo(t *testing.T, fixture string) string {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join("testdata", fixture, "tree")
	err := filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dir, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, content, 0o644)
	})
	if err != nil {
		t.Fatalf("copying fixture tree %s: %v", source, err)
	}
	initRepo(t, dir)
	return dir
}

func newRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	initRepo(t, dir)
	return dir
}

func initRepo(t *testing.T, dir string) {
	t.Helper()
	git(t, dir, "init", "-q")
	git(t, dir, "config", "user.email", "test@example.com")
	git(t, dir, "config", "user.name", "Test")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "initial", "--allow-empty")
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func readFixtureDiff(t *testing.T, fixture string) []byte {
	t.Helper()
	diff, err := os.ReadFile(filepath.Join("testdata", fixture, "change.diff"))
	if err != nil {
		t.Fatalf("reading fixture diff: %v", err)
	}
	return diff
}

// runWithScript plays a fixed list of model responses, so every test here is
// about what inspector does with what a model says, never about the model.
func runWithScript(t *testing.T, opts AgentOptions, responses ...string) (Result, error) {
	t.Helper()
	calls := 0
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls > len(responses) {
			t.Errorf("unexpected model call %d", calls)
			return
		}
		_, _ = w.Write([]byte(responses[calls-1]))
	}))
	defer model.Close()

	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	opts.Model = "test-model"
	opts.APIBaseURL = model.URL
	return RunAgent(context.Background(), opts)
}

func toolCall(id, name string, input map[string]any) string {
	encoded, err := json.Marshal(input)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf(`{"content":[{"type":"tool_use","id":%q,"name":%q,"input":%s}]}`, id, name, encoded)
}

func findingCall(id string, finding map[string]any) string {
	return toolCall(id, "report_documentation_findings", map[string]any{"findings": []map[string]any{finding}})
}

var finishCall = toolCall("finish-1", "finish_documentation_review", map[string]any{})

// pr74Finding is the stale list in AGENTS.md, which the change never touched.
// The claim is quoted exactly as the fixture has it, spanning two lines.
func pr74Finding() map[string]any {
	return map[string]any{
		"kind":       string(FalsifiedClaim),
		"path":       "AGENTS.md",
		"line":       6,
		"claim":      "There are four of them: `runBuild`, `runCheck`,\n`runDeploy`, and `runStatus`.",
		"summary":    "the change added runMigrate and runRollback, so the command layer now has six entry points and this list names four of them",
		"correction": "There are six of them: `runBuild`, `runCheck`, `runDeploy`, `runMigrate`, `runRollback`, and `runStatus`.",
	}
}

// TestRunAgent_FindsAnIncompleteListInAFileTheChangeNeverTouched is issue
// #16's definition of done, in the shape of the case that prompted it
// (Fabrica PR #74): the change added two entry points and correctly updated
// the README that lists them, while a list of four in AGENTS.md - a file not
// in the change at all - became incomplete. Nothing that looks only at the
// diff can find this.
func TestRunAgent_FindsAnIncompleteListInAFileTheChangeNeverTouched(t *testing.T) {
	repo := newFixtureRepo(t, "pr74")
	result, err := runWithScript(t,
		AgentOptions{Diff: readFixtureDiff(t, "pr74"), RepoPath: repo, Request: "add migrate and rollback commands"},
		toolCall("search-1", "search_project", map[string]any{"query": "runStatus"}),
		toolCall("read-1", "read_file", map[string]any{"path": "AGENTS.md"}),
		findingCall("report-1", pr74Finding()),
		finishCall,
	)
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if result.Kind != Reviewed || result.Verdict != DocumentationFalsified {
		t.Fatalf("result = %#v, want a completed review with verdict %q", result, DocumentationFalsified)
	}
	if len(result.Findings) != 1 {
		t.Fatalf("findings = %#v, want exactly one", result.Findings)
	}
	finding := result.Findings[0]
	if finding.Path != "AGENTS.md" || finding.Line != 6 {
		t.Fatalf("finding = %#v, want it pointed at AGENTS.md:6", finding)
	}
	if finding.InChange {
		// The whole point: this file is not in the diff, and inspector read
		// that fact from the diff rather than taking the model's word for it.
		t.Fatalf("finding.InChange = true, want false - AGENTS.md is not in the change")
	}
	if finding.Correction == "" {
		t.Fatalf("finding = %#v, want a proposed correction", finding)
	}
	if result.Lookups != 2 {
		t.Fatalf("Lookups = %d, want 2 - the search and the read that found this", result.Lookups)
	}
	if !result.RequestProvided {
		t.Fatal("RequestProvided = false, want true - a request was given")
	}
}

func TestRunAgent_SearchFindsTheStaleListFromASiblingName(t *testing.T) {
	// The mechanism the case depends on: searching for the names the change
	// ADDED finds nothing in AGENTS.md, and searching for the name it now
	// sits beside finds the stale list.
	corpus, err := LoadCorpus(newFixtureRepo(t, "pr74"))
	if err != nil {
		t.Fatalf("LoadCorpus() error = %v", err)
	}
	if got := corpus.Search("runMigrate"); strings.Contains(got, "AGENTS.md") {
		t.Fatalf("searching the added name reached AGENTS.md: %s", got)
	}
	got := corpus.Search("runStatus")
	if !strings.Contains(got, "AGENTS.md:7") {
		t.Fatalf("search for a sibling name = %q, want a hit in AGENTS.md", got)
	}
}

func TestRunAgent_CleanReviewIsAResult(t *testing.T) {
	result, err := runWithScript(t,
		AgentOptions{Diff: readFixtureDiff(t, "pr74"), RepoPath: newFixtureRepo(t, "pr74")},
		toolCall("search-1", "search_project", map[string]any{"query": "runStatus"}),
		finishCall,
	)
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if result.Verdict != DocumentationConsistent || len(result.Findings) != 0 {
		t.Fatalf("result = %#v, want a clean completed review", result)
	}
	if result.RequestProvided {
		t.Fatal("RequestProvided = true, want false - no request was given")
	}
}

func TestRunAgent_CannotFinishWithoutLookingAtTheProject(t *testing.T) {
	// A review that read nothing outside the change never asked the second
	// question, and from the outside that is indistinguishable from one that
	// asked and found nothing. The first finish is refused; the second, after
	// a lookup, completes.
	result, err := runWithScript(t,
		AgentOptions{Diff: readFixtureDiff(t, "pr74"), RepoPath: newFixtureRepo(t, "pr74")},
		finishCall,
		toolCall("read-1", "read_file", map[string]any{"path": "AGENTS.md"}),
		finishCall,
	)
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if result.Kind != Reviewed || result.Lookups != 1 {
		t.Fatalf("result = %#v, want a review completed only after one lookup", result)
	}
}

func TestRunAgent_NeverFinishesWithoutALookup(t *testing.T) {
	responses := make([]string, maxAgentTurns)
	for i := range responses {
		responses[i] = finishCall
	}
	_, err := runWithScript(t,
		AgentOptions{Diff: readFixtureDiff(t, "pr74"), RepoPath: newFixtureRepo(t, "pr74")},
		responses...,
	)
	if err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("RunAgent() error = %v, want a refusal rather than a review that looked at nothing", err)
	}
}

func TestRunAgent_RejectsAFindingWhoseQuotedClaimIsNotInTheFile(t *testing.T) {
	finding := pr74Finding()
	finding["claim"] = "There are seven of them, which this file never said"
	result, err := runWithScript(t,
		AgentOptions{Diff: readFixtureDiff(t, "pr74"), RepoPath: newFixtureRepo(t, "pr74")},
		toolCall("read-1", "read_file", map[string]any{"path": "AGENTS.md"}),
		findingCall("report-1", finding),
		finishCall,
	)
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if len(result.Findings) != 0 || result.Verdict != DocumentationConsistent {
		t.Fatalf("result = %#v, want the unverifiable claim rejected rather than recorded", result)
	}
}

func TestRunAgent_RejectsAFindingPointedAtTheWrongLine(t *testing.T) {
	finding := pr74Finding()
	finding["line"] = 1
	result, err := runWithScript(t,
		AgentOptions{Diff: readFixtureDiff(t, "pr74"), RepoPath: newFixtureRepo(t, "pr74")},
		toolCall("read-1", "read_file", map[string]any{"path": "AGENTS.md"}),
		findingCall("report-1", finding),
		finishCall,
	)
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if len(result.Findings) != 0 {
		t.Fatalf("findings = %#v, want a pointer nobody can follow rejected", result.Findings)
	}
}

func TestRunAgent_RejectsAFindingNamingADocumentTheProjectNeverHad(t *testing.T) {
	// SPEC.md section 4's first limit, enforced rather than requested:
	// deciding the project needs a guide it has never had is the Client's.
	finding := pr74Finding()
	finding["path"] = "docs/COMMANDS.md"
	result, err := runWithScript(t,
		AgentOptions{Diff: readFixtureDiff(t, "pr74"), RepoPath: newFixtureRepo(t, "pr74")},
		toolCall("read-1", "read_file", map[string]any{"path": "AGENTS.md"}),
		findingCall("report-1", finding),
		finishCall,
	)
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if len(result.Findings) != 0 {
		t.Fatalf("findings = %#v, want a proposed new document rejected", result.Findings)
	}
}

func TestRunAgent_RecordsAClaimThatNeedsTheClientsJudgment(t *testing.T) {
	finding := pr74Finding()
	delete(finding, "correction")
	finding["needs_client_judgment"] = true
	finding["judgment_reason"] = "whether rollback belongs in this list at all is a product call, not a wording fix"
	result, err := runWithScript(t,
		AgentOptions{Diff: readFixtureDiff(t, "pr74"), RepoPath: newFixtureRepo(t, "pr74")},
		toolCall("read-1", "read_file", map[string]any{"path": "AGENTS.md"}),
		findingCall("report-1", finding),
		finishCall,
	)
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	flagged := result.NeedsClientJudgment()
	if len(flagged) != 1 || flagged[0].JudgmentReason == "" {
		t.Fatalf("flagged = %#v, want one finding carrying its reason", flagged)
	}
	if result.Verdict != DocumentationFalsified {
		t.Fatalf("verdict = %q, want the falsified claim to still count", result.Verdict)
	}
}

func TestRunAgent_RejectsAFindingThatIsNeitherCorrectedNorFlagged(t *testing.T) {
	finding := pr74Finding()
	delete(finding, "correction")
	result, err := runWithScript(t,
		AgentOptions{Diff: readFixtureDiff(t, "pr74"), RepoPath: newFixtureRepo(t, "pr74")},
		toolCall("read-1", "read_file", map[string]any{"path": "AGENTS.md"}),
		findingCall("report-1", finding),
		finishCall,
	)
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if len(result.Findings) != 0 {
		t.Fatalf("findings = %#v, want silence about what to do rejected", result.Findings)
	}
}

func TestRunAgent_FindsAnUndescribedChangeInADocumentThatCoversIt(t *testing.T) {
	repo := newFixtureRepo(t, "pr74")
	result, err := runWithScript(t,
		AgentOptions{Diff: readFixtureDiff(t, "pr74"), RepoPath: repo},
		toolCall("read-1", "read_file", map[string]any{"path": "src/cli/README.md"}),
		findingCall("report-1", map[string]any{
			"kind":       string(UndescribedChange),
			"path":       "src/cli/README.md",
			"line":       3,
			"claim":      "Commands are registered in `commands.go`, one entry point each:",
			"summary":    "the two new commands are listed but the README never says migrate must run before rollback",
			"correction": "Commands are registered in `commands.go`, one entry point each, and `runRollback` undoes whatever `runMigrate` last applied:",
		}),
		finishCall,
	)
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if result.Verdict != DocumentationUndescribed {
		t.Fatalf("verdict = %q, want %q", result.Verdict, DocumentationUndescribed)
	}
	if !result.Findings[0].InChange {
		t.Fatalf("finding = %#v, want InChange true - this file is in the change", result.Findings[0])
	}
}

func TestRunAgent_RefusesWithoutAModel(t *testing.T) {
	if _, err := RunAgent(context.Background(), AgentOptions{Diff: readFixtureDiff(t, "pr74")}); err == nil {
		t.Fatal("RunAgent() succeeded with no model configured")
	}
}

func TestRunAgent_RefusesOnAnEmptyChange(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	_, err := RunAgent(context.Background(), AgentOptions{Diff: []byte(""), Model: "test-model"})
	if err == nil || !strings.Contains(err.Error(), "no files") {
		t.Fatalf("RunAgent() error = %v, want a refusal naming the empty change", err)
	}
}

func TestRunAgent_RefusesWhenThereIsNoProjectToReRead(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	_, err := RunAgent(context.Background(), AgentOptions{
		Diff: readFixtureDiff(t, "pr74"), RepoPath: t.TempDir(), Model: "test-model",
	})
	if err == nil || !strings.Contains(err.Error(), "git repository") {
		t.Fatalf("RunAgent() error = %v, want a refusal rather than a review of nothing", err)
	}
}

func TestRunAgent_OpenAICompatibleVendorRecordsAFinding(t *testing.T) {
	repo := newFixtureRepo(t, "pr74")
	calls := 0
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		switch calls {
		case 1:
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"read-1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"AGENTS.md\"}"}}]}}]}`))
		case 2:
			encoded, err := json.Marshal(map[string]any{"findings": []map[string]any{pr74Finding()}})
			if err != nil {
				t.Fatalf("encoding finding: %v", err)
			}
			arguments, err := json.Marshal(string(encoded))
			if err != nil {
				t.Fatalf("encoding arguments: %v", err)
			}
			fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"report-1","type":"function","function":{"name":"report_documentation_findings","arguments":%s}}]}}]}`, arguments)
		case 3:
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"finish-1","type":"function","function":{"name":"finish_documentation_review","arguments":"{}"}}]}}]}`))
		default:
			t.Errorf("unexpected model call %d", calls)
		}
	}))
	defer model.Close()

	t.Setenv("FAKEVENDOR_API_KEY", "test-key")
	t.Setenv("FAKEVENDOR_BASE_URL", model.URL)
	result, err := RunAgent(context.Background(), AgentOptions{
		Diff: readFixtureDiff(t, "pr74"), RepoPath: repo, Vendor: "fakevendor", Model: "test-model",
	})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if len(result.Findings) != 1 {
		t.Fatalf("result = %#v, want one finding via the OpenAI-compatible vendor", result)
	}
}

func TestRunAgent_UnavailableToolIsRefusedWithoutEndingTheReview(t *testing.T) {
	result, err := runWithScript(t,
		AgentOptions{Diff: readFixtureDiff(t, "pr74"), RepoPath: newFixtureRepo(t, "pr74")},
		toolCall("write-1", "write_file", map[string]any{"path": "AGENTS.md", "content": "fixed"}),
		toolCall("read-1", "read_file", map[string]any{"path": "AGENTS.md"}),
		finishCall,
	)
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	// There is no tool that writes, and asking for one costs the model a turn
	// rather than the run.
	if result.Kind != Reviewed || result.Lookups != 1 {
		t.Fatalf("result = %#v, want the unavailable tool refused and the review completed", result)
	}
}
