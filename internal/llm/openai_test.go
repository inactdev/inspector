package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenAIAdapter_Ask_SendsAndParsesToolCalls(t *testing.T) {
	var gotRequest openAIRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("Authorization = %q, want Bearer test-key", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&gotRequest); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"thinking","tool_calls":[{"id":"call-1","type":"function","function":{"name":"do_thing","arguments":"{\"x\":1}"}}]}}]}`))
	}))
	defer server.Close()

	adapter := &OpenAIAdapter{APIKey: "test-key", Model: "test-model", BaseURL: server.URL}
	blocks, err := adapter.Ask(context.Background(), "be helpful", []Tool{
		{Name: "do_thing", Description: "does a thing", InputSchema: map[string]any{"type": "object"}},
	}, []Message{
		{Role: "user", Content: []Block{{Type: Text, Text: "hello"}}},
	})
	if err != nil {
		t.Fatalf("Ask() error = %v", err)
	}
	if len(gotRequest.Messages) != 2 || gotRequest.Messages[0].Role != "system" || *gotRequest.Messages[0].Content != "be helpful" {
		t.Fatalf("request messages = %#v, want a leading system message", gotRequest.Messages)
	}
	if gotRequest.Messages[1].Role != "user" || *gotRequest.Messages[1].Content != "hello" {
		t.Fatalf("request messages[1] = %#v, want the user turn", gotRequest.Messages[1])
	}
	if len(gotRequest.Tools) != 1 || gotRequest.Tools[0].Type != "function" || gotRequest.Tools[0].Function.Name != "do_thing" {
		t.Fatalf("request tools = %#v, want a function tool named do_thing", gotRequest.Tools)
	}
	if len(blocks) != 2 || blocks[0].Type != Text || blocks[0].Text != "thinking" {
		t.Fatalf("blocks = %#v, want a leading text block", blocks)
	}
	if blocks[1].Type != ToolUse || blocks[1].ToolUseID != "call-1" || blocks[1].ToolName != "do_thing" || string(blocks[1].ToolInput) != `{"x":1}` {
		t.Fatalf("blocks[1] = %#v, want the parsed tool call", blocks[1])
	}
}

func TestOpenAIAdapter_Ask_SplitsPackedToolResultsIntoToolMessages(t *testing.T) {
	var gotRequest openAIRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotRequest); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer server.Close()

	adapter := &OpenAIAdapter{APIKey: "test-key", Model: "test-model", BaseURL: server.URL}
	_, err := adapter.Ask(context.Background(), "", nil, []Message{
		{Role: "assistant", Content: []Block{
			{Type: ToolUse, ToolUseID: "call-1", ToolName: "a", ToolInput: json.RawMessage(`{}`)},
			{Type: ToolUse, ToolUseID: "call-2", ToolName: "b", ToolInput: json.RawMessage(`{}`)},
		}},
		{Role: "user", Content: []Block{
			{Type: ToolResult, ToolUseID: "call-1", ToolResult: "result a"},
			{Type: ToolResult, ToolUseID: "call-2", ToolResult: "result b"},
		}},
	})
	if err != nil {
		t.Fatalf("Ask() error = %v", err)
	}
	// system prompt is empty so it is skipped: assistant turn, then two
	// separate tool-role messages, one per packed Anthropic-style tool_result.
	if len(gotRequest.Messages) != 3 {
		t.Fatalf("messages = %#v, want 3", gotRequest.Messages)
	}
	assistant := gotRequest.Messages[0]
	if assistant.Role != "assistant" || len(assistant.ToolCalls) != 2 {
		t.Fatalf("assistant message = %#v, want two tool_calls", assistant)
	}
	first, second := gotRequest.Messages[1], gotRequest.Messages[2]
	if first.Role != "tool" || first.ToolCallID != "call-1" || *first.Content != "result a" {
		t.Fatalf("first tool message = %#v, want call-1's result", first)
	}
	if second.Role != "tool" || second.ToolCallID != "call-2" || *second.Content != "result b" {
		t.Fatalf("second tool message = %#v, want call-2's result", second)
	}
}

func TestOpenAIAdapter_Ask_RefusesWithoutAPIKeyOrBaseURL(t *testing.T) {
	if _, err := (&OpenAIAdapter{Model: "m", BaseURL: "http://127.0.0.1:1"}).Ask(context.Background(), "", nil, nil); err == nil {
		t.Fatal("Ask() succeeded without an API key")
	}
	if _, err := (&OpenAIAdapter{APIKey: "key", Model: "m"}).Ask(context.Background(), "", nil, nil); err == nil {
		t.Fatal("Ask() succeeded without a base URL")
	}
}
