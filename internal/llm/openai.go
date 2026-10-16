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

// OpenAIAdapter speaks the OpenAI-compatible chat-completions request shape
// (POST {BaseURL}/chat/completions, tools-as-functions, a "tool" role for
// results). It works against any vendor serving that shape at a configurable
// address - OpenAI itself, xAI, OpenRouter, a local Ollama - never one vendor
// only.
type OpenAIAdapter struct {
	APIKey  string
	Model   string
	BaseURL string
}

type openAIRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	Tools     []openAITool    `json:"tools,omitempty"`
	Messages  []openAIMessage `json:"messages"`
}

type openAITool struct {
	Type     string         `json:"type"`
	Function openAIFunction `json:"function"`
}

type openAIFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    *string          `json:"content"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIToolCall struct {
	ID       string            `json:"id"`
	Type     string            `json:"type"`
	Function openAICallPayload `json:"function"`
}

type openAICallPayload struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAIResponse struct {
	Choices []struct {
		Message openAIMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Ask implements Adapter.
func (a *OpenAIAdapter) Ask(ctx context.Context, system string, tools []Tool, messages []Message) ([]Block, error) {
	if strings.TrimSpace(a.APIKey) == "" {
		return nil, fmt.Errorf("openai-compatible adapter has no API key")
	}
	if strings.TrimSpace(a.BaseURL) == "" {
		return nil, fmt.Errorf("openai-compatible adapter has no base URL")
	}
	converted := make([]openAIMessage, 0, len(messages)+1)
	if strings.TrimSpace(system) != "" {
		converted = append(converted, openAIMessage{Role: "system", Content: stringPtr(system)})
	}
	for _, message := range messages {
		converted = append(converted, openAIMessages(message)...)
	}
	body, err := json.Marshal(openAIRequest{
		Model:     a.Model,
		MaxTokens: 4096,
		Tools:     openAITools(tools),
		Messages:  converted,
	})
	if err != nil {
		return nil, fmt.Errorf("encoding model request: %w", err)
	}
	endpoint := strings.TrimRight(a.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building model request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.APIKey)
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
	var result openAIResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("decoding model response: %w", err)
	}
	if result.Error != nil {
		return nil, fmt.Errorf("model returned an error: %s", result.Error.Message)
	}
	if len(result.Choices) == 0 {
		return nil, fmt.Errorf("model response has no choices")
	}
	return blocksFromOpenAI(result.Choices[0].Message), nil
}

func openAITools(tools []Tool) []openAITool {
	if len(tools) == 0 {
		return nil
	}
	converted := make([]openAITool, len(tools))
	for i, tool := range tools {
		converted[i] = openAITool{Type: "function", Function: openAIFunction{
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  tool.InputSchema,
		}}
	}
	return converted
}

// openAIMessages expands one generic message into the OpenAI-compatible
// shape: a user message carrying only text stays one "user" message, but a
// user message carrying tool results becomes one "tool" message per result,
// each keyed by its tool_call_id - the shape this API requires and Anthropic's
// packed tool_result blocks do not.
func openAIMessages(message Message) []openAIMessage {
	var text strings.Builder
	var toolCalls []openAIToolCall
	var toolResults []openAIMessage
	for _, block := range message.Content {
		switch block.Type {
		case Text:
			if text.Len() > 0 {
				text.WriteString("\n")
			}
			text.WriteString(block.Text)
		case ToolUse:
			toolCalls = append(toolCalls, openAIToolCall{
				ID:   block.ToolUseID,
				Type: "function",
				Function: openAICallPayload{
					Name:      block.ToolName,
					Arguments: string(block.ToolInput),
				},
			})
		case ToolResult:
			toolResults = append(toolResults, openAIMessage{
				Role:       "tool",
				Content:    stringPtr(block.ToolResult),
				ToolCallID: block.ToolUseID,
			})
		}
	}
	if len(toolResults) > 0 {
		return toolResults
	}
	message2 := openAIMessage{Role: message.Role}
	if text.Len() > 0 || len(toolCalls) == 0 {
		message2.Content = stringPtr(text.String())
	}
	message2.ToolCalls = toolCalls
	return []openAIMessage{message2}
}

func blocksFromOpenAI(message openAIMessage) []Block {
	var blocks []Block
	if message.Content != nil && strings.TrimSpace(*message.Content) != "" {
		blocks = append(blocks, Block{Type: Text, Text: *message.Content})
	}
	for _, call := range message.ToolCalls {
		blocks = append(blocks, Block{
			Type:      ToolUse,
			ToolUseID: call.ID,
			ToolName:  call.Function.Name,
			ToolInput: json.RawMessage(call.Function.Arguments),
		})
	}
	return blocks
}

func stringPtr(s string) *string { return &s }
