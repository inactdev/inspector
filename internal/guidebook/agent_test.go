package guidebook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// runWithScript plays a fixed list of model responses, so every test here is
// about what inspector does with what a model says, never about the model.
func runWithScript(t *testing.T, repo string, responses ...string) (Result, []byte, error) {
	t.Helper()
	calls := 0
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls > len(responses) {
			t.Errorf("unexpected model call %d", calls)
			return
		}
		_, _ = w.Write([]byte(responses[calls-1]))
	}))
	defer model.Close()

	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	return RunAgent(context.Background(), AgentOptions{
		RepoPath:   repo,
		Model:      "test-model",
		APIBaseURL: model.URL,
	})
}

func toolCall(id, name string, input map[string]any) string {
	encoded, err := json.Marshal(input)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf(`{"content":[{"type":"tool_use","id":%q,"name":%q,"input":%s}]}`, id, name, encoded)
}

var (
	lookupCall = toolCall("read-1", "read_file", map[string]any{"path": "server.js"})
	finishCall = toolCall("finish-1", "finish_guidebook", map[string]any{})
)

func sectionsCall(fact string) string {
	sections := make([]map[string]any, 0, len(RequiredSections))
	for _, section := range completeSections(fact) {
		sections = append(sections, map[string]any{
			"name": string(section.Name), "body": section.Body, "groundedIn": section.GroundedIn,
		})
	}
	return toolCall("sections-1", "write_sections", map[string]any{"sections": sections})
}

func featuresCall(fact string, names ...string) string {
	features := make([]map[string]any, 0, len(names))
	for _, name := range names {
		feature := completeFeature(name, fact)
		features = append(features, map[string]any{
			"name": feature.Name, "subFeatures": feature.SubFeatures,
			"howToReachIt": feature.HowToReachIt, "drivingIt": feature.DrivingIt,
			"gotchas": feature.Gotchas, "groundedIn": feature.GroundedIn,
		})
	}
	return toolCall("features-1", "write_features", map[string]any{"features": features})
}

// firstFactID is the id of the first fact inspector extracts from the fixture,
// which the scripted model cites the way a real one would.
func firstFactID(t *testing.T, repo string) string {
	t.Helper()
	_, facts, err := Prepare(repo)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if len(facts.Facts) == 0 {
		t.Fatal("the fixture project produced no facts")
	}
	return facts.Facts[0].ID
}

// TestRunAgent_WritesAGuidebookForAProjectItHasNeverSeen is issue #22's done:
// pointed at a repository, it produces the document an examiner with no source
// can work from.
func TestRunAgent_WritesAGuidebookForAProjectItHasNeverSeen(t *testing.T) {
	repo := newRepo(t, webApp)
	fact := firstFactID(t, repo)
	result, document, err := runWithScript(t, repo,
		toolCall("search-1", "search_project", map[string]any{"query": "app.get"}),
		lookupCall,
		sectionsCall(fact),
		featuresCall(fact, "Baskets", "Accounts"),
		finishCall,
	)
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if result.Kind != Generated {
		t.Fatalf("result = %#v, want a generated guidebook", result)
	}
	if result.Project != "orchard" || result.Commit == "" {
		t.Errorf("result names project %q at commit %q", result.Project, result.Commit)
	}
	if len(result.Features) != 2 {
		t.Errorf("features = %v, want both recorded", result.Features)
	}
	if result.Lookups != 2 {
		t.Errorf("Lookups = %d, want the two times it opened the project", result.Lookups)
	}
	if result.Facts == 0 || len(result.FactsBy) == 0 {
		t.Error("the result does not report what grounded the guidebook")
	}
	text := string(document)
	for _, name := range RequiredSections {
		if !strings.Contains(text, "## "+string(name)) {
			t.Errorf("the document has no %q section", name)
		}
	}
}

// TestRunAgent_WillNotFinishWithoutOpeningTheProject is the guard against the
// design this rejected: a guidebook written from the extracted facts alone is a
// template, produced at model prices.
func TestRunAgent_WillNotFinishWithoutOpeningTheProject(t *testing.T) {
	repo := newRepo(t, webApp)
	fact := firstFactID(t, repo)
	_, _, err := runWithScript(t, repo,
		sectionsCall(fact),
		featuresCall(fact, "Baskets"),
		finishCall,
		// A fourth response is never reached if the finish is rejected and the
		// model stops producing tool calls, so the loop ends in an error.
		`{"content":[{"type":"text","text":"done"}]}`,
	)
	if err == nil || !strings.Contains(err.Error(), "neither read the project") {
		t.Fatalf("RunAgent() error = %v, want the run to have refused to finish on an unread project", err)
	}
}

// TestRunAgent_RejectsAnIncompleteGuidebookAtFinish keeps a half-written
// document from reaching a file somebody would mistake for a guidebook.
func TestRunAgent_RejectsAnIncompleteGuidebookAtFinish(t *testing.T) {
	repo := newRepo(t, webApp)
	fact := firstFactID(t, repo)
	sections := completeSections(fact)
	partial := make([]map[string]any, 0, len(sections)-1)
	for _, section := range sections[:len(sections)-1] {
		partial = append(partial, map[string]any{"name": string(section.Name), "body": section.Body, "groundedIn": section.GroundedIn})
	}
	_, _, err := runWithScript(t, repo,
		lookupCall,
		toolCall("sections-1", "write_sections", map[string]any{"sections": partial}),
		featuresCall(fact, "Baskets"),
		finishCall,
		`{"content":[{"type":"text","text":"stopping"}]}`,
	)
	if err == nil {
		t.Fatal("RunAgent() returned a guidebook with a section missing")
	}
}

// TestRunAgent_RecoversAfterARejectedProposal - a rejection is information the
// model can act on, not the end of the run.
func TestRunAgent_RecoversAfterARejectedProposal(t *testing.T) {
	repo := newRepo(t, webApp)
	fact := firstFactID(t, repo)
	result, _, err := runWithScript(t, repo,
		lookupCall,
		toolCall("features-0", "write_features", map[string]any{"features": []map[string]any{{
			"name": "Baskets", "subFeatures": "x", "howToReachIt": "y", "drivingIt": "z",
			"gotchas": "w", "groundedIn": []string{"f9999"},
		}}}),
		sectionsCall(fact),
		featuresCall(fact, "Baskets"),
		finishCall,
	)
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if len(result.Features) != 1 || result.Features[0] != "Baskets" {
		t.Errorf("features = %v, want the corrected proposal recorded once", result.Features)
	}
}

func TestRunAgent_RefusesWithoutAModel(t *testing.T) {
	_, _, err := RunAgent(context.Background(), AgentOptions{RepoPath: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "no model was configured") {
		t.Fatalf("RunAgent() error = %v, want a refusal naming the missing model", err)
	}
}

func TestRunAgent_StopsAfterItsTurnBudget(t *testing.T) {
	repo := newRepo(t, webApp)
	responses := make([]string, 0, maxAgentTurns)
	for n := 0; n < maxAgentTurns; n++ {
		responses = append(responses, lookupCall)
	}
	_, _, err := runWithScript(t, repo, responses...)
	if err == nil || !strings.Contains(err.Error(), "did not finish the guidebook") {
		t.Fatalf("RunAgent() error = %v, want the turn budget enforced", err)
	}
}
