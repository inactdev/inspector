package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/inactdev/inspector/internal/llm"
)

const maxAgentTurns = 16

// AgentOptions configures one review run. It has no repository path or app
// URL by design: the reviewer never mounts a project or drives an app.
type AgentOptions struct {
	Diff []byte
	// TestPathPatterns are the caller's globs naming which files in the
	// change are tests (issue #44). Empty means inspector was never told,
	// which the result reports as such rather than as "no test changes".
	TestPathPatterns []string
	Vendor           string // raw --vendor flag value; empty selects Anthropic.
	Model            string
	APIBaseURL       string // test-only Anthropic override; inspector review never accepts one.
}

// RunAgent asks the model to read the change and report what looks wrong.
// The model never submits a final verdict beyond its own findings; the
// machine validates every finding against the change actually under review.
func RunAgent(ctx context.Context, opts AgentOptions) (Result, error) {
	if strings.TrimSpace(opts.Model) == "" {
		return Result{}, errors.New("no model was configured")
	}
	adapter, _, err := llm.Resolve(opts.Vendor, opts.Model)
	if err != nil {
		return Result{}, err
	}
	if anthropic, ok := adapter.(*llm.AnthropicAdapter); ok && opts.APIBaseURL != "" {
		anthropic.BaseURL = opts.APIBaseURL
	}
	files, err := ParseUnifiedDiff(opts.Diff)
	if err != nil {
		return Result{}, fmt.Errorf("parsing change: %w", err)
	}
	if len(files) == 0 {
		return Result{}, errors.New("the change has no files to review")
	}
	testChanges, err := ChangedTests(files, opts.TestPathPatterns)
	if err != nil {
		return Result{}, fmt.Errorf("selecting the change's test files: %w", err)
	}
	record := NewRecord(Paths(files), testChanges)
	judgmentRequested := len(opts.TestPathPatterns) > 0

	initial := fmt.Sprintf("CHANGE (the number in the left column of each line is what you must cite; review it the way a careful colleague would):\n\n%s", RenderForReview(files))
	system, tools := agentSystemPrompt, agentTools
	if len(testChanges) > 0 {
		initial = RenderTestChanges(testChanges) + initial
		system += "\n\n" + testChangeDuty
		tools = append(append([]llm.Tool{}, agentTools...), judgeTestChangeTool)
	}
	messages := []llm.Message{{Role: "user", Content: []llm.Block{{Type: llm.Text, Text: initial}}}}

	for turn := 0; turn < maxAgentTurns; turn++ {
		blocks, err := adapter.Ask(ctx, system, tools, messages)
		if err != nil {
			return Result{}, err
		}
		if len(blocks) == 0 {
			return Result{}, errors.New("model returned no content")
		}
		messages = append(messages, llm.Message{Role: "assistant", Content: blocks})

		toolResults := make([]llm.Block, 0, len(blocks))
		// finish_review is answered after every other call in the same
		// turn, so a judgment the model made alongside it counts, but its
		// result still sits where the model put it: each tool_use needs its
		// tool_result back in the order it was made.
		finishIndex, finishID := -1, ""
		for _, block := range blocks {
			if block.Type != llm.ToolUse {
				continue
			}
			switch block.ToolName {
			case "report_findings":
				toolResults = append(toolResults, toolResult(block.ToolUseID, reportFindings(record, block.ToolInput)))
			case "judge_test_change":
				toolResults = append(toolResults, toolResult(block.ToolUseID, judgeTestChange(record, block.ToolInput)))
			case "finish_review":
				if finishIndex >= 0 {
					toolResults = append(toolResults, toolResult(block.ToolUseID, "rejected: finish_review was already called in this turn"))
					continue
				}
				finishIndex, finishID = len(toolResults), block.ToolUseID
				toolResults = append(toolResults, llm.Block{})
			default:
				toolResults = append(toolResults, toolResult(block.ToolUseID, "tool is not available"))
			}
		}
		finish := false
		if finishIndex >= 0 {
			// A changed test nobody judged must never be able to end the
			// review looking like one that held up.
			if unjudged := record.UnjudgedTestPaths(); len(unjudged) > 0 {
				toolResults[finishIndex] = toolResult(finishID, "rejected: every changed test file needs a judge_test_change judgment first; still unjudged: "+strings.Join(unjudged, ", "))
			} else {
				finish = true
				toolResults[finishIndex] = toolResult(finishID, "review recorded")
			}
		}
		if len(toolResults) == 0 {
			return Result{}, errors.New("model did not report findings or finish the review")
		}
		messages = append(messages, llm.Message{Role: "user", Content: toolResults})
		if finish {
			return Result{
				Kind:              Reviewed,
				Findings:          record.Findings,
				TestChangeVerdict: DeriveTestChangeVerdict(judgmentRequested, record.TestChanges),
				TestChanges:       record.TestChanges,
			}, nil
		}
	}
	if unjudged := record.UnjudgedTestPaths(); len(unjudged) > 0 {
		return Result{}, fmt.Errorf("model did not judge every changed test file within %d turns; unjudged: %s", maxAgentTurns, strings.Join(unjudged, ", "))
	}
	return Result{}, fmt.Errorf("model did not finish the review within %d turns", maxAgentTurns)
}

func reportFindings(record *Record, input json.RawMessage) string {
	var request struct {
		Findings []Finding `json:"findings"`
	}
	if err := json.Unmarshal(input, &request); err != nil {
		return "rejected: decoding report_findings call: " + err.Error()
	}
	if err := record.AddFindings(request.Findings); err != nil {
		return "rejected: " + err.Error()
	}
	return fmt.Sprintf("recorded %d finding(s)", len(request.Findings))
}

func judgeTestChange(record *Record, input json.RawMessage) string {
	var request struct {
		Path     string       `json:"path"`
		Judgment TestJudgment `json:"judgment"`
		Reason   string       `json:"reason"`
	}
	if err := json.Unmarshal(input, &request); err != nil {
		return "rejected: decoding judge_test_change call: " + err.Error()
	}
	if err := record.JudgeTestChange(request.Path, request.Judgment, request.Reason); err != nil {
		return "rejected: " + err.Error()
	}
	return fmt.Sprintf("recorded %s for %s", request.Judgment, request.Path)
}

const agentSystemPrompt = `You are Inspector's independent code reviewer. Read CHANGE, a unified diff annotated with explicit line numbers in its left column, and report anything that looks wrong - bugs, security issues, missed edge cases, inconsistencies with the rest of the change - the way a careful colleague reviewing a pull request would. You never run the application and never edit anything; reading the change is the whole job. Point every finding at the exact file and line number shown in CHANGE's left column. A removed line has no line number of its own; for something wrong about removed code, cite the nearest shown line and say so in the summary. Call report_findings for everything you find - as many times as you like, including zero times if nothing looks wrong. When you are done, call finish_review.`

// testChangeDuty is added to the system prompt only when the change actually
// touches a test file. The duty exists because the most ordinary thing a
// builder under pressure does is make a failing check stop failing, and a
// test is the cheapest place to do it - so a change to the exam itself needs
// saying out loud rather than being read as ordinary code.
const testChangeDuty = `The change also touches test files, listed under CHANGED TESTS above. For each one, judge whether the change keeps the exam that test was imposing or weakens it, and call judge_test_change once per path with one of:

- "weakens_the_exam": the change removes, loosens, skips, narrows, or deletes something the test was proving, so code that would have failed before can now pass. Deleting a test, dropping an assertion, widening an accepted value, marking a test skipped, and replacing a specific expectation with a vaguer one all belong here. Judge what the test proves, not whether the change looks tidy or deliberate - a well-written weakening is still a weakening.
- "keeps_the_exam_intact": the test still proves at least what it proved before, or it is new coverage proving something additional. A test rewritten to assert the same thing a different way belongs here.
- "cannot_tell": the diff alone does not let you judge it. Use this honestly rather than guessing either way; it is recorded as unjudged, never as intact.

Say in the reason what the test proved and what happened to that. Judging every listed path is required before you can finish.`

var judgeTestChangeTool = llm.Tool{
	Name:        "judge_test_change",
	Description: "Judge one changed test file from CHANGED TESTS: does the change keep the exam that test imposed, or weaken it? Call this once per listed path.",
	InputSchema: map[string]any{"type": "object", "properties": map[string]any{
		"path":     map[string]any{"type": "string", "description": "one of the paths listed under CHANGED TESTS"},
		"judgment": map[string]any{"type": "string", "enum": []string{string(ExamWeakened), string(ExamIntact), string(ExamUnclear)}},
		"reason":   map[string]any{"type": "string", "description": "what the test proved and what this change did to that"},
	}, "required": []string{"path", "judgment", "reason"}},
}

var agentTools = []llm.Tool{
	{
		Name:        "report_findings",
		Description: "Report one or more things that look wrong, each pointing at its exact file and line number from CHANGE's left column.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"findings": map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "object", "properties": map[string]any{
				"path":    map[string]any{"type": "string"},
				"line":    map[string]any{"type": "integer"},
				"summary": map[string]any{"type": "string"},
			}, "required": []string{"path", "line", "summary"}}},
		}, "required": []string{"findings"}},
	},
	{
		Name:        "finish_review",
		Description: "Finish the review. Call this once every finding has been reported, including when there are none, and every file listed under CHANGED TESTS has been judged.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	},
}

func toolResult(id, content string) llm.Block {
	return llm.Block{Type: llm.ToolResult, ToolUseID: id, ToolResult: content}
}
