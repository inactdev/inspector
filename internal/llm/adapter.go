// Package llm is Inspector's vendor seam: one small interface for "ask a
// model with these tools," so no caller - the examiner, the code reviewer, or
// the documentation judgment - is written against one vendor's request shape.
package llm

import (
	"context"
	"encoding/json"
)

// maxResponseBytes bounds a vendor API response read by any adapter.
const maxResponseBytes = 1 << 20

// Tool describes one function the model may call. InputSchema is a JSON
// Schema object describing its arguments.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// BlockType names what one piece of message content carries.
type BlockType string

const (
	// Text is plain model or caller text.
	Text BlockType = "text"
	// ToolUse is a tool call the model made: ToolUseID, ToolName, and
	// ToolInput (the call's JSON arguments) are set.
	ToolUse BlockType = "tool_use"
	// ToolResult is the result of a tool call fed back to the model:
	// ToolUseID and ToolResult are set.
	ToolResult BlockType = "tool_result"
)

// Block is one piece of a message's content.
type Block struct {
	Type BlockType

	// Text holds the content for a Text block.
	Text string

	// ToolUseID identifies the tool call a ToolUse block made or a
	// ToolResult block answers.
	ToolUseID string
	// ToolName and ToolInput are set on a ToolUse block.
	ToolName  string
	ToolInput json.RawMessage
	// ToolResult is set on a ToolResult block.
	ToolResult string
}

// Message is one turn of the conversation.
type Message struct {
	Role    string // "user" or "assistant"
	Content []Block
}

// Adapter asks one vendor's model to use the given tools over the given
// conversation and returns what it said, including any tools it called.
// Every adapter speaks this one shape; vendor request/response translation
// stays inside the adapter.
type Adapter interface {
	Ask(ctx context.Context, system string, tools []Tool, messages []Message) ([]Block, error)
}
