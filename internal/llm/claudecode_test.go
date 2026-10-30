package llm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeClaude writes a stand-in for the `claude` command that records the
// argument list it was given and the prompt it was handed on stdin, then
// prints body as its `--output-format json` envelope. Nothing here talks to a
// model: what is under test is the command line Inspector builds, the prompt
// it renders, and what it makes of the answer.
func fakeClaude(t *testing.T, body string) (command, argsPath, stdinPath string) {
	t.Helper()
	dir := t.TempDir()
	command = filepath.Join(dir, "claude")
	argsPath = filepath.Join(dir, "args")
	stdinPath = filepath.Join(dir, "stdin")
	// NUL-separated, because two of the arguments under test - the schema and
	// the appended protocol - contain newlines of their own.
	script := "#!/bin/sh\n" +
		"printf '%s\\000' \"$@\" > " + argsPath + "\n" +
		"cat > " + stdinPath + "\n" +
		"cat <<'CLAUDE_EOF'\n" + body + "\nCLAUDE_EOF\n"
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatalf("writing the stand-in command: %v", err)
	}
	return command, argsPath, stdinPath
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

func envelope(t *testing.T, structured string) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"type":              "result",
		"is_error":          false,
		"result":            structured,
		"structured_output": json.RawMessage(structured),
	})
	if err != nil {
		t.Fatalf("building the envelope: %v", err)
	}
	return string(data)
}

func TestClaudeCodeAdapter_RunsTheCommandAndReadsItsBlocks(t *testing.T) {
	command, argsPath, stdinPath := fakeClaude(t, envelope(t, `{"blocks":[{"type":"text","text":"here is what I found"},{"type":"tool_use","tool_name":"report_findings","tool_input":{"findings":[{"path":"a.go"}]}},{"type":"tool_use","tool_name":"finish_review"}]}`))
	adapter := &ClaudeCodeAdapter{Model: "claude-sonnet-5", Command: command}

	tools := []Tool{
		{Name: "report_findings", Description: "Report what looks wrong.", InputSchema: map[string]any{"type": "object"}},
		{Name: "finish_review", Description: "Finish the review.", InputSchema: map[string]any{}},
	}
	messages := []Message{{Role: "user", Content: []Block{{Type: Text, Text: "CHANGE: a.go line 1"}}}}
	blocks, err := adapter.Ask(context.Background(), "You are Inspector's reviewer.", tools, messages)
	if err != nil {
		t.Fatalf("Ask() error = %v", err)
	}

	want := []Block{
		{Type: Text, Text: "here is what I found"},
		{Type: ToolUse, ToolUseID: "claude-code-0", ToolName: "report_findings", ToolInput: json.RawMessage(`{"findings":[{"path":"a.go"}]}`)},
		{Type: ToolUse, ToolUseID: "claude-code-1", ToolName: "finish_review", ToolInput: json.RawMessage(`{}`)},
	}
	if len(blocks) != len(want) {
		t.Fatalf("Ask() returned %d blocks, want %d: %+v", len(blocks), len(want), blocks)
	}
	for i, block := range blocks {
		if block.Type != want[i].Type || block.Text != want[i].Text || block.ToolUseID != want[i].ToolUseID || block.ToolName != want[i].ToolName {
			t.Fatalf("block %d = %+v, want %+v", i, block, want[i])
		}
		if string(block.ToolInput) != string(want[i].ToolInput) {
			t.Fatalf("block %d input = %s, want %s", i, block.ToolInput, want[i].ToolInput)
		}
	}

	// The command line is the containment, so it is asserted rather than
	// assumed: every built-in tool off, the host's customizations off, no MCP
	// server, no session on disk, the caller's model, and the caller's own
	// system prompt.
	args := strings.Split(strings.TrimSuffix(readFile(t, argsPath), "\x00"), "\x00")
	assertArgPair(t, args, "--output-format", "json")
	assertArgPair(t, args, "--model", "claude-sonnet-5")
	assertArgPair(t, args, "--tools", "")
	assertArgPair(t, args, "--system-prompt", "You are Inspector's reviewer.")
	assertArgPair(t, args, "--append-system-prompt", claudeCodeProtocol)
	assertArgPair(t, args, "--json-schema", claudeCodeSchema)
	for _, flag := range []string{"--print", "--safe-mode", "--strict-mcp-config", "--disable-slash-commands", "--no-session-persistence"} {
		if indexOf(args, flag) < 0 {
			t.Fatalf("the command line is missing %s: %q", flag, args)
		}
	}

	// No caller text in argv: the whole conversation arrives on stdin.
	prompt := readFile(t, stdinPath)
	for _, want := range []string{"report_findings", "Report what looks wrong.", "INPUT SCHEMA: {\"type\":\"object\"}", "finish_review", "### user", "CHANGE: a.go line 1"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("the prompt does not carry %q:\n%s", want, prompt)
		}
	}
}

// TestClaudeCodeAdapter_ToolUseIDsContinueTheConversation covers the one thing
// the model is deliberately not trusted with. A caller feeds each tool_use ID
// back as the ID of its tool result, so two calls sharing an ID - or a call
// with none - would silently answer the wrong one.
func TestClaudeCodeAdapter_ToolUseIDsContinueTheConversation(t *testing.T) {
	command, _, _ := fakeClaude(t, envelope(t, `{"blocks":[{"type":"tool_use","tool_name":"finish_review"}]}`))
	adapter := &ClaudeCodeAdapter{Model: "claude-sonnet-5", Command: command}
	messages := []Message{
		{Role: "user", Content: []Block{{Type: Text, Text: "CHANGE"}}},
		{Role: "assistant", Content: []Block{
			{Type: ToolUse, ToolUseID: "claude-code-0", ToolName: "report_findings", ToolInput: json.RawMessage(`{}`)},
			{Type: ToolUse, ToolUseID: "claude-code-1", ToolName: "judge_test_change", ToolInput: json.RawMessage(`{}`)},
		}},
		{Role: "user", Content: []Block{
			{Type: ToolResult, ToolUseID: "claude-code-0", ToolResult: "recorded 1 finding(s)"},
			{Type: ToolResult, ToolUseID: "claude-code-1", ToolResult: "rejected: unknown path"},
		}},
	}
	blocks, err := adapter.Ask(context.Background(), "system", nil, messages)
	if err != nil {
		t.Fatalf("Ask() error = %v", err)
	}
	if len(blocks) != 1 || blocks[0].ToolUseID != "claude-code-2" {
		t.Fatalf("Ask() = %+v, want one tool_use with ID claude-code-2", blocks)
	}
}

func TestRenderClaudeCodePrompt_CarriesToolResultsBackToTheirCall(t *testing.T) {
	prompt := renderClaudeCodePrompt(nil, []Message{
		{Role: "assistant", Content: []Block{{Type: ToolUse, ToolUseID: "claude-code-0", ToolName: "report_findings", ToolInput: json.RawMessage(`{"findings":[]}`)}}},
		{Role: "user", Content: []Block{{Type: ToolResult, ToolUseID: "claude-code-0", ToolResult: "rejected: findings must not be empty"}}},
	})
	for _, want := range []string{
		"[tool_use claude-code-0] report_findings {\"findings\":[]}",
		"[TOOL RESULT for claude-code-0]\nrejected: findings must not be empty",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("the prompt does not carry %q:\n%s", want, prompt)
		}
	}
}

func TestClaudeCodeAdapter_NamesWhatTheCommandSaidWentWrong(t *testing.T) {
	failing := `{"type":"result","is_error":true,"result":"There's an issue with the selected model (nope-5).","api_error_status":404}`
	for name, body := range map[string]string{
		"an error envelope":        failing,
		"no structured output":     `{"type":"result","is_error":false,"result":"There's an issue with the selected model (nope-5)."}`,
		"a non-result envelope":    `{"type":"system","is_error":false}`,
		"an unknown block type":    envelope(t, `{"blocks":[{"type":"thinking","text":"hm"}]}`),
		"a nameless tool call":     envelope(t, `{"blocks":[{"type":"tool_use","tool_input":{}}]}`),
		"no blocks at all":         envelope(t, `{"blocks":[]}`),
		"output that is not JSON":  "claude: command not configured",
		"an unparseable structure": `{"type":"result","is_error":false,"structured_output":{"blocks":"all of them"}}`,
	} {
		command, _, _ := fakeClaude(t, body)
		adapter := &ClaudeCodeAdapter{Model: "claude-sonnet-5", Command: command}
		if _, err := adapter.Ask(context.Background(), "system", nil, []Message{{Role: "user", Content: []Block{{Type: Text, Text: "hi"}}}}); err == nil {
			t.Fatalf("Ask() with %s returned no error", name)
		} else if name == "an error envelope" && !strings.Contains(err.Error(), "nope-5") {
			t.Fatalf("Ask() error = %v, want the command's own message about nope-5", err)
		}
	}
}

// TestClaudeCodeAdapter_FailedCommandQuotesItsOwnMessage covers the shape the
// real command actually fails in: a non-zero exit AND an error envelope on
// stdout. The envelope's message is the useful half.
func TestClaudeCodeAdapter_FailedCommandQuotesItsOwnMessage(t *testing.T) {
	dir := t.TempDir()
	command := filepath.Join(dir, "claude")
	script := "#!/bin/sh\ncat > /dev/null\n" +
		`printf '%s' '{"type":"result","is_error":true,"result":"Invalid API key · Please run /login"}'` + "\nexit 1\n"
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatalf("writing the stand-in command: %v", err)
	}
	adapter := &ClaudeCodeAdapter{Model: "claude-sonnet-5", Command: command}
	_, err := adapter.Ask(context.Background(), "system", nil, []Message{{Role: "user", Content: []Block{{Type: Text, Text: "hi"}}}})
	if err == nil || !strings.Contains(err.Error(), "Please run /login") {
		t.Fatalf("Ask() error = %v, want the command's own login message", err)
	}
}

func TestClaudeCodeAdapter_BoundsOneAsk(t *testing.T) {
	dir := t.TempDir()
	command := filepath.Join(dir, "claude")
	if err := os.WriteFile(command, []byte("#!/bin/sh\ncat > /dev/null\nsleep 30\n"), 0o755); err != nil {
		t.Fatalf("writing the stand-in command: %v", err)
	}
	adapter := &ClaudeCodeAdapter{Model: "claude-sonnet-5", Command: command, Timeout: 100 * time.Millisecond}
	start := time.Now()
	_, err := adapter.Ask(context.Background(), "system", nil, []Message{{Role: "user", Content: []Block{{Type: Text, Text: "hi"}}}})
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("Ask() error = %v, want a timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 25*time.Second {
		t.Fatalf("Ask() waited %s for a command it had bounded to 100ms", elapsed)
	}
}

func TestClaudeCodeAdapter_BoundsWhatItKeepsOfTheAnswer(t *testing.T) {
	buffer := &boundedBuffer{limit: 4}
	// The whole write has to read as accepted, or the command is stopped
	// with a write error instead of being allowed to finish and be judged.
	if n, err := buffer.Write([]byte("abcdef")); n != 6 || err != nil {
		t.Fatalf("Write() = (%d, %v), want (6, nil)", n, err)
	}
	if string(buffer.data) != "abcd" {
		t.Fatalf("kept %q, want only the first 4 bytes", buffer.data)
	}
	if _, err := parseClaudeCodeEnvelope(make([]byte, maxResponseBytes+1)); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("parseClaudeCodeEnvelope() error = %v, want it to name the byte limit", err)
	}
}

func TestClaudeCodeAdapter_RefusesWithoutAModel(t *testing.T) {
	adapter := &ClaudeCodeAdapter{}
	if _, err := adapter.Ask(context.Background(), "system", nil, nil); err == nil || !strings.Contains(err.Error(), "no model") {
		t.Fatalf("Ask() error = %v, want a refusal naming the missing model", err)
	}
}

func assertArgPair(t *testing.T, args []string, flag, value string) {
	t.Helper()
	at := indexOf(args, flag)
	if at < 0 {
		t.Fatalf("the command line is missing %s: %q", flag, args)
	}
	if at+1 >= len(args) || args[at+1] != value {
		got := ""
		if at+1 < len(args) {
			got = args[at+1]
		}
		t.Fatalf("%s = %q, want %q", flag, got, value)
	}
}

func indexOf(args []string, want string) int {
	for i, arg := range args {
		if arg == want {
			return i
		}
	}
	return -1
}
