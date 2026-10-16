package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultAnthropicURL = "https://api.anthropic.com/v1/messages"

// AnthropicAdapter speaks Anthropic's Messages API. BaseURL is a test-only
// override; production callers always use the fixed Anthropic address.
type AnthropicAdapter struct {
	APIKey  string
	Model   string
	BaseURL string
}

type anthropicMessageRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system"`
	Tools     []anthropicTool    `json:"tools"`
	Messages  []anthropicMessage `json:"messages"`
}

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type anthropicResponse struct {
	Content []anthropicBlock `json:"content"`
}

type anthropicBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	Text  string          `json:"text,omitempty"`
}

// Ask implements Adapter.
func (a *AnthropicAdapter) Ask(ctx context.Context, system string, tools []Tool, messages []Message) ([]Block, error) {
	if strings.TrimSpace(a.APIKey) == "" {
		return nil, fmt.Errorf("anthropic adapter has no API key")
	}
	body, err := json.Marshal(anthropicMessageRequest{
		Model:     a.Model,
		MaxTokens: 4096,
		System:    system,
		Tools:     anthropicTools(tools),
		Messages:  anthropicMessages(messages),
	})
	if err != nil {
		return nil, fmt.Errorf("encoding model request: %w", err)
	}
	endpoint := a.BaseURL
	if endpoint == "" {
		endpoint = defaultAnthropicURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building model request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", a.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	client := &http.Client{
		Timeout: 60 * time.Second,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			return fmt.Errorf("model API redirected to %s", req.URL.Redacted())
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling model: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading model response: %w", err)
	}
	if len(data) > maxResponseBytes {
		return nil, fmt.Errorf("model response exceeds the %d byte limit", maxResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("model returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var result anthropicResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("decoding model response: %w", err)
	}
	return blocksFromAnthropic(result.Content), nil
}

func anthropicTools(tools []Tool) []anthropicTool {
	converted := make([]anthropicTool, len(tools))
	for i, tool := range tools {
		converted[i] = anthropicTool{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema}
	}
	return converted
}

// anthropicMessages converts the generic conversation into Anthropic's wire
// shape. The mapping is close to 1:1: Anthropic's own content blocks are
// already named "text", "tool_use", and "tool_result".
func anthropicMessages(messages []Message) []anthropicMessage {
	converted := make([]anthropicMessage, len(messages))
	for i, message := range messages {
		converted[i] = anthropicMessage{Role: message.Role, Content: anthropicContent(message.Content)}
	}
	return converted
}

func anthropicContent(blocks []Block) any {
	if len(blocks) == 1 && blocks[0].Type == Text {
		return blocks[0].Text
	}
	content := make([]map[string]any, len(blocks))
	for i, block := range blocks {
		switch block.Type {
		case ToolUse:
			content[i] = map[string]any{"type": "tool_use", "id": block.ToolUseID, "name": block.ToolName, "input": block.ToolInput}
		case ToolResult:
			content[i] = map[string]any{"type": "tool_result", "tool_use_id": block.ToolUseID, "content": block.ToolResult}
		default:
			content[i] = map[string]any{"type": "text", "text": block.Text}
		}
	}
	return content
}

func blocksFromAnthropic(blocks []anthropicBlock) []Block {
	converted := make([]Block, len(blocks))
	for i, block := range blocks {
		switch block.Type {
		case "tool_use":
			converted[i] = Block{Type: ToolUse, ToolUseID: block.ID, ToolName: block.Name, ToolInput: block.Input}
		case "text":
			converted[i] = Block{Type: Text, Text: block.Text}
		default:
			converted[i] = Block{Type: BlockType(block.Type), Text: block.Text}
		}
	}
	return converted
}
