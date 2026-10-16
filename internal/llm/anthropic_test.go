package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAnthropicAdapter_Ask_SendsAndParsesToolUse(t *testing.T) {
	var gotRequest anthropicMessageRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("x-api-key = %q, want test-key", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Errorf("anthropic-version header was not set")
		}
		if err := json.NewDecoder(r.Body).Decode(&gotRequest); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"thinking"},{"type":"tool_use","id":"call-1","name":"do_thing","input":{"x":1}}]}`))
	}))
	defer server.Close()

	adapter := &AnthropicAdapter{APIKey: "test-key", Model: "test-model", BaseURL: server.URL}
	blocks, err := adapter.Ask(context.Background(), "be helpful", []Tool{
		{Name: "do_thing", Description: "does a thing", InputSchema: map[string]any{"type": "object"}},
	}, []Message{
		{Role: "user", Content: []Block{{Type: Text, Text: "hello"}}},
	})
	if err != nil {
		t.Fatalf("Ask() error = %v", err)
	}
	if gotRequest.System != "be helpful" || gotRequest.Model != "test-model" {
		t.Fatalf("request = %#v, want the system prompt and model forwarded", gotRequest)
	}
	if len(gotRequest.Tools) != 1 || gotRequest.Tools[0].Name != "do_thing" {
		t.Fatalf("request tools = %#v, want do_thing", gotRequest.Tools)
	}
	if len(blocks) != 2 || blocks[0].Type != Text || blocks[0].Text != "thinking" {
		t.Fatalf("blocks = %#v, want a leading text block", blocks)
	}
	if blocks[1].Type != ToolUse || blocks[1].ToolUseID != "call-1" || blocks[1].ToolName != "do_thing" {
		t.Fatalf("blocks[1] = %#v, want the parsed tool_use call", blocks[1])
	}
}

func TestAnthropicAdapter_Ask_RoundTripsToolResultMessages(t *testing.T) {
	var messages []json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []json.RawMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		messages = body.Messages
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
	defer server.Close()

	adapter := &AnthropicAdapter{APIKey: "test-key", Model: "test-model", BaseURL: server.URL}
	_, err := adapter.Ask(context.Background(), "", nil, []Message{
		{Role: "assistant", Content: []Block{{Type: ToolUse, ToolUseID: "call-1", ToolName: "do_thing", ToolInput: json.RawMessage(`{"x":1}`)}}},
		{Role: "user", Content: []Block{{Type: ToolResult, ToolUseID: "call-1", ToolResult: "it worked"}}},
	})
	if err != nil {
		t.Fatalf("Ask() error = %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(messages))
	}
	if string(messages[1]) != `{"role":"user","content":[{"content":"it worked","tool_use_id":"call-1","type":"tool_result"}]}` {
		t.Fatalf("tool_result message = %s, want the packed Anthropic shape", messages[1])
	}
}
