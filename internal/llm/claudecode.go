package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ClaudeCodeCommand is the executable this vendor runs. It is looked up on
// PATH and never built, downloaded, or installed.
const ClaudeCodeCommand = "claude"

// defaultClaudeCodeTimeout bounds one Ask. The HTTP adapters bound a request
// with their client timeout; a local command needs the same bound for the
// same reason - a caller whose context has no deadline (both reviewers pass
// context.Background()) must not be able to wait forever.
const defaultClaudeCodeTimeout = 5 * time.Minute

// ClaudeCodeAdapter asks the model through the local `claude` command run
// non-interactively, for the one environment where a subscription login is the
// only model access there is: no API key, no base URL, nothing for Inspector
// to read or forward. See notes/20261030-a-vendor-that-runs-the-claude-command.md.
type ClaudeCodeAdapter struct {
	// Model is passed straight through as `--model`, so the vendor's model
	// setting means here what it means everywhere else.
	Model string
	// Command overrides the executable. Tests point it at a stand-in;
	// production callers leave it empty and get ClaudeCodeCommand from PATH.
	Command string
	// Timeout bounds one Ask, defaulting to defaultClaudeCodeTimeout.
	Timeout time.Duration
}

// claudeCodeEnvelope is the `--output-format json` result object. Only the
// four fields Inspector judges by are read: everything else in it (cost,
// token usage, session id) is this command's own accounting.
type claudeCodeEnvelope struct {
	Type             string          `json:"type"`
	IsError          bool            `json:"is_error"`
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output"`
}

type claudeCodeBlocks struct {
	Blocks []claudeCodeBlock `json:"blocks"`
}

type claudeCodeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
}

// Ask implements Adapter. One Ask is one `claude --print` run carrying the
// whole conversation, the same way one Ask is one HTTP request for the other
// adapters: the command is never left running between turns and never resumes
// a session.
func (a *ClaudeCodeAdapter) Ask(ctx context.Context, system string, tools []Tool, messages []Message) ([]Block, error) {
	command := a.Command
	if strings.TrimSpace(command) == "" {
		command = ClaudeCodeCommand
	}
	if strings.TrimSpace(a.Model) == "" {
		return nil, fmt.Errorf("claude-code adapter has no model")
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = defaultClaudeCodeTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// The command runs in a throwaway empty directory, never the project
	// being judged: nothing of the judged tree is read from, or written
	// into, the place a judgment happens to be started from.
	workdir, err := os.MkdirTemp("", "inspector-claude-code-")
	if err != nil {
		return nil, fmt.Errorf("preparing the claude working directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(workdir) }()

	cmd := exec.CommandContext(runCtx, command, claudeCodeArgs(a.Model, system)...)
	cmd.Dir = workdir
	cmd.Stdin = strings.NewReader(renderClaudeCodePrompt(tools, messages))
	// Bounded like the HTTP adapters' io.LimitReader, and for the same
	// reason: how much a model's answer may occupy is Inspector's decision,
	// not the answering end's.
	stdout := &boundedBuffer{limit: maxResponseBytes + 1}
	stderr := &boundedBuffer{limit: maxStderrBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// Kill-then-wait rather than wait forever: a killed command's children
	// can still hold the output pipe open, and os/exec only stops waiting on
	// it once WaitDelay elapses.
	cmd.WaitDelay = time.Second
	runErr := cmd.Run()
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("the %s command ran past its %s timeout", command, timeout)
	}
	if runCtx.Err() != nil {
		return nil, fmt.Errorf("the %s command was interrupted: %w", command, runCtx.Err())
	}

	envelope, parseErr := parseClaudeCodeEnvelope(stdout.data)
	if runErr != nil {
		// The command's own message is worth more than its exit status, and
		// it arrives on stdout inside the envelope rather than on stderr.
		if parseErr == nil && strings.TrimSpace(envelope.Result) != "" {
			return nil, fmt.Errorf("the %s command failed: %s", command, strings.TrimSpace(envelope.Result))
		}
		if detail := strings.TrimSpace(string(stderr.data)); detail != "" {
			return nil, fmt.Errorf("running the %s command: %v: %s", command, runErr, detail)
		}
		return nil, fmt.Errorf("running the %s command: %w", command, runErr)
	}
	if parseErr != nil {
		return nil, parseErr
	}
	if envelope.IsError {
		return nil, fmt.Errorf("the %s command reported an error: %s", command, claudeCodeDetail(envelope))
	}
	if envelope.Type != "result" {
		return nil, fmt.Errorf("the %s command returned %q, not a result", command, envelope.Type)
	}
	if len(envelope.StructuredOutput) == 0 {
		return nil, fmt.Errorf("the %s command returned no structured output: %s", command, claudeCodeDetail(envelope))
	}
	return blocksFromClaudeCode(envelope.StructuredOutput, messages)
}

// claudeCodeArgs is the whole command line, kept in one place so the
// containment it carries can be read at once. Every tool, customization and
// MCP server the host may have is turned off: the model is asked to read and
// answer, and nothing else. The prompt itself goes on stdin, so no caller
// text ever lands in argv - and `--tools` is variadic, which would swallow a
// trailing positional argument anyway.
func claudeCodeArgs(model, system string) []string {
	return []string{
		"--print",
		"--output-format", "json",
		"--model", model,
		// No built-in tool: the only tools in play are the caller's, and
		// Inspector answers those itself. Claude Code's own Bash/Edit/Write
		// must never run - Inspector never edits what it judges.
		"--tools", "",
		// No hooks, CLAUDE.md, skills, plugins, MCP servers or custom agents
		// from the host. Deliberately not --bare, which would read only
		// ANTHROPIC_API_KEY and never the subscription login this vendor
		// exists for.
		"--safe-mode",
		"--strict-mcp-config",
		"--disable-slash-commands",
		// The judged change is not session history.
		"--no-session-persistence",
		"--json-schema", claudeCodeSchema,
		"--system-prompt", system,
		"--append-system-prompt", claudeCodeProtocol,
	}
}

// claudeCodeSchema is the JSON Schema the command validates its answer
// against: exactly the Adapter's own []Block, narrowed to the two kinds a
// model can produce.
const claudeCodeSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["blocks"],
  "properties": {
    "blocks": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["type"],
        "properties": {
          "type": {"type": "string", "enum": ["text", "tool_use"]},
          "text": {"type": "string"},
          "tool_name": {"type": "string"},
          "tool_input": {"type": "object"}
        }
      }
    }
  }
}`

// claudeCodeProtocol is appended to the caller's own system prompt. It is the
// only part of the conversation this adapter writes: it says how to answer,
// never what to conclude.
const claudeCodeProtocol = `You are being asked through a non-interactive interface that gives you no tools of your own and no way to ask a question. Your entire reply is one JSON object matching the given schema:

{"blocks": [ ... ]}

Each block is one of:

- {"type": "text", "text": "..."} - something you want to say.
- {"type": "tool_use", "tool_name": "<a name listed under TOOLS>", "tool_input": { ... }} - a call to one of the tools listed under TOOLS in the message below, with arguments matching that tool's INPUT SCHEMA exactly.

Wherever your instructions tell you to call a tool, emit a tool_use block for it. You may emit several blocks in one reply. The caller runs each call and sends you its result as a TOOL RESULT in the next message, so a rejected call is one you can correct and make again. Never name a tool that is not listed under TOOLS, never wrap the JSON in prose or code fences, and never answer with a question - nothing here is interactive.`

// renderClaudeCodePrompt turns the tool definitions and the conversation into
// the one prompt the command reads on stdin. The transcript is rendered in
// full on every Ask because each run is a fresh session - the same thing a
// stateless HTTP request does, and the reason this vendor is not the cheapest
// one.
func renderClaudeCodePrompt(tools []Tool, messages []Message) string {
	var prompt strings.Builder
	if len(tools) > 0 {
		prompt.WriteString("TOOLS (the only tools you may call; call one by emitting a tool_use block naming it):\n")
		for _, tool := range tools {
			fmt.Fprintf(&prompt, "\n## %s\n\n%s\n\nINPUT SCHEMA: %s\n", tool.Name, tool.Description, renderToolSchema(tool.InputSchema))
		}
		prompt.WriteString("\n")
	}
	prompt.WriteString("CONVERSATION (every turn so far; answer the last one):\n")
	for _, message := range messages {
		fmt.Fprintf(&prompt, "\n### %s\n", message.Role)
		for _, block := range message.Content {
			switch block.Type {
			case ToolUse:
				fmt.Fprintf(&prompt, "\n[tool_use %s] %s %s\n", block.ToolUseID, block.ToolName, strings.TrimSpace(string(block.ToolInput)))
			case ToolResult:
				fmt.Fprintf(&prompt, "\n[TOOL RESULT for %s]\n%s\n", block.ToolUseID, block.ToolResult)
			default:
				fmt.Fprintf(&prompt, "\n%s\n", block.Text)
			}
		}
	}
	return prompt.String()
}

func renderToolSchema(schema map[string]any) string {
	if len(schema) == 0 {
		return "{}"
	}
	data, err := json.Marshal(schema)
	if err != nil {
		// A schema Inspector itself wrote and cannot encode is a programming
		// error, not a model problem; say so in the prompt rather than
		// failing the whole judgment over it.
		return fmt.Sprintf("{} (this tool's schema could not be rendered: %v)", err)
	}
	return string(data)
}

// maxStderrBytes bounds what is kept of the command's stderr. It is only ever
// quoted in an error message.
const maxStderrBytes = 8 << 10

// boundedBuffer collects at most limit bytes and silently drops the rest. A
// truncated envelope fails to decode, which is the same answer the HTTP
// adapters give an over-long response, reached without holding it all.
type boundedBuffer struct {
	data  []byte
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - len(b.data); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		b.data = append(b.data, p[:room]...)
	}
	// The whole write is reported as accepted: a short count would stop the
	// command with a write error instead of letting it finish and be judged.
	return len(p), nil
}

func parseClaudeCodeEnvelope(data []byte) (claudeCodeEnvelope, error) {
	if len(data) > maxResponseBytes {
		return claudeCodeEnvelope{}, fmt.Errorf("model response exceeds the %d byte limit", maxResponseBytes)
	}
	var envelope claudeCodeEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return claudeCodeEnvelope{}, fmt.Errorf("decoding model response: %w", err)
	}
	return envelope, nil
}

// claudeCodeDetail is what to quote when the envelope carries no blocks: its
// own result text if it has any, and otherwise a note that it did not say.
func claudeCodeDetail(envelope claudeCodeEnvelope) string {
	if detail := strings.TrimSpace(envelope.Result); detail != "" {
		return detail
	}
	return "it gave no reason"
}

// blocksFromClaudeCode converts the validated answer into Adapter blocks. The
// tool_use IDs are Inspector's, counted on from the calls already in the
// conversation, so the model can neither omit one nor collide with an earlier
// one - the caller feeds these IDs back as tool results and nothing else has
// to be true of them.
func blocksFromClaudeCode(structured json.RawMessage, messages []Message) ([]Block, error) {
	var answer claudeCodeBlocks
	if err := json.Unmarshal(structured, &answer); err != nil {
		return nil, fmt.Errorf("decoding the model's blocks: %w", err)
	}
	nextID := countToolUses(messages)
	blocks := make([]Block, 0, len(answer.Blocks))
	for _, block := range answer.Blocks {
		switch block.Type {
		case "tool_use":
			if strings.TrimSpace(block.ToolName) == "" {
				return nil, fmt.Errorf("the model asked for a tool call without naming a tool")
			}
			input := block.ToolInput
			if len(input) == 0 {
				// A tool whose schema takes no argument - finish_review -
				// is the common case here, and an absent object is how a
				// model spells it.
				input = json.RawMessage("{}")
			}
			blocks = append(blocks, Block{
				Type:      ToolUse,
				ToolUseID: fmt.Sprintf("claude-code-%d", nextID),
				ToolName:  block.ToolName,
				ToolInput: input,
			})
			nextID++
		case "text":
			blocks = append(blocks, Block{Type: Text, Text: block.Text})
		default:
			return nil, fmt.Errorf("the model answered with an unknown block type %q", block.Type)
		}
	}
	if len(blocks) == 0 {
		return nil, fmt.Errorf("the model answered with no blocks")
	}
	return blocks, nil
}

func countToolUses(messages []Message) int {
	count := 0
	for _, message := range messages {
		for _, block := range message.Content {
			if block.Type == ToolUse {
				count++
			}
		}
	}
	return count
}
