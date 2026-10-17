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
	Diff       []byte
	Vendor     string // raw --vendor flag value; empty selects Anthropic.
	Model      string
	APIBaseURL string // test-only Anthropic override; inspector review never accepts one.
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
	record := NewRecord(Paths(files))

	initial := fmt.Sprintf("CHANGE (the number in the left column of each line is what you must cite; review it the way a careful colleague would):\n\n%s", RenderForReview(files))
	messages := []llm.Message{{Role: "user", Content: []llm.Block{{Type: llm.Text, Text: initial}}}}

	for turn := 0; turn < maxAgentTurns; turn++ {
		blocks, err := adapter.Ask(ctx, agentSystemPrompt, agentTools, messages)
		if err != nil {
			return Result{}, err
		}
		if len(blocks) == 0 {
			return Result{}, errors.New("model returned no content")
		}
		messages = append(messages, llm.Message{Role: "assistant", Content: blocks})

		toolResults := make([]llm.Block, 0, len(blocks))
		finish := false
		for _, block := range blocks {
			if block.Type != llm.ToolUse {
				continue
			}
			switch block.ToolName {
			case "report_findings":
				toolResults = append(toolResults, toolResult(block.ToolUseID, reportFindings(record, block.ToolInput)))
			case "finish_review":
				finish = true
				toolResults = append(toolResults, toolResult(block.ToolUseID, "review recorded"))
			default:
				toolResults = append(toolResults, toolResult(block.ToolUseID, "tool is not available"))
			}
		}
		if len(toolResults) == 0 {
			return Result{}, errors.New("model did not report findings or finish the review")
		}
		messages = append(messages, llm.Message{Role: "user", Content: toolResults})
		if finish {
			return Result{Kind: Reviewed, Findings: record.Findings}, nil
		}
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

const agentSystemPrompt = `You are Inspector's independent code reviewer. Read CHANGE, a unified diff annotated with explicit line numbers in its left column, and report anything that looks wrong - bugs, security issues, missed edge cases, inconsistencies with the rest of the change - the way a careful colleague reviewing a pull request would. You never run the application and never edit anything; reading the change is the whole job. Point every finding at the exact file and line number shown in CHANGE's left column. A removed line has no line number of its own; for something wrong about removed code, cite the nearest shown line and say so in the summary. Call report_findings for everything you find - as many times as you like, including zero times if nothing looks wrong. When you are done, call finish_review.`

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
		Description: "Finish the review. Call this once every finding has been reported, including when there are none.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	},
}

func toolResult(id, content string) llm.Block {
	return llm.Block{Type: llm.ToolResult, ToolUseID: id, ToolResult: content}
}
