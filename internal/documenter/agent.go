package documenter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/inactdev/inspector/internal/llm"
	"github.com/inactdev/inspector/internal/reviewer"
)

// maxAgentTurns is higher than the code reviewer's, because the second
// question is answered by going and looking: deriving the names a claim could
// be about, searching the project for each, and reading what comes back costs
// turns the reviewer never spends.
const maxAgentTurns = 24

// AgentOptions configures one documentation review.
type AgentOptions struct {
	// Diff is the change, as a unified diff. It answers "what changed"; it is
	// never where the stale document is found.
	Diff []byte
	// RepoPath is any path inside the project whose documentation to re-read.
	// Inspector resolves its working-tree root and builds the corpus from the
	// tracked files there.
	RepoPath string
	// Request is the request text or expected-outcome list. It is the only
	// permitted source of what was meant (SPEC.md section 4); empty means
	// inspector was told nothing, which the result reports as such.
	Request    string
	Vendor     string // raw --vendor flag value; empty selects Anthropic.
	Model      string
	APIBaseURL string // test-only Anthropic override; inspector document never accepts one.
}

// RunAgent asks the model both documentation questions over the project's own
// tracked text. The model proposes findings; inspector validates every one
// against the corpus and derives the verdict itself.
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
	// The diff parser and its line annotation are the code reviewer's, reused
	// rather than written twice: a second dialect of "which line is this"
	// across two judgments of the same change is a bug waiting for a reader.
	files, err := reviewer.ParseUnifiedDiff(opts.Diff)
	if err != nil {
		return Result{}, fmt.Errorf("parsing change: %w", err)
	}
	if len(files) == 0 {
		return Result{}, errors.New("the change has no files, so there is nothing whose documentation could have gone stale")
	}
	corpus, err := LoadCorpus(opts.RepoPath)
	if err != nil {
		return Result{}, err
	}

	record := NewRecord(corpus, reviewer.Paths(files))
	requestProvided := strings.TrimSpace(opts.Request) != ""
	system := agentSystemPrompt
	if requestProvided {
		system += "\n\n" + requestGivenDuty
	} else {
		system += "\n\n" + noRequestDuty
	}

	var initial strings.Builder
	if requestProvided {
		fmt.Fprintf(&initial, "REQUEST (the only thing that says what was meant; never infer intent from the implementation):\n\n%s\n\n", strings.TrimSpace(opts.Request))
	} else {
		initial.WriteString("REQUEST: none was given to inspector. You therefore have no account of what was meant, and must not invent one.\n\n")
	}
	initial.WriteString(corpus.Describe())
	fmt.Fprintf(&initial, "CHANGE (what the change did; the documentation that went stale is usually NOT in here):\n\n%s", reviewer.RenderForReview(files))
	messages := []llm.Message{{Role: "user", Content: []llm.Block{{Type: llm.Text, Text: initial.String()}}}}

	lookups := 0
	for turn := 0; turn < maxAgentTurns; turn++ {
		blocks, err := adapter.Ask(ctx, system, agentTools, messages)
		if err != nil {
			return Result{}, err
		}
		if len(blocks) == 0 {
			return Result{}, errors.New("model returned no content")
		}
		messages = append(messages, llm.Message{Role: "assistant", Content: blocks})

		toolResults := make([]llm.Block, 0, len(blocks))
		// finish is answered after every other call in the same turn, so a
		// lookup or a report the model made alongside it counts - but its
		// result still sits where the model put it, since each tool_use needs
		// its tool_result back in the order it was made.
		finishIndex, finishID := -1, ""
		for _, block := range blocks {
			if block.Type != llm.ToolUse {
				continue
			}
			switch block.ToolName {
			case "search_project":
				lookups++
				toolResults = append(toolResults, toolResult(block.ToolUseID, searchProject(corpus, block.ToolInput)))
			case "read_file":
				lookups++
				toolResults = append(toolResults, toolResult(block.ToolUseID, readFile(corpus, block.ToolInput)))
			case "list_files":
				lookups++
				toolResults = append(toolResults, toolResult(block.ToolUseID, listFiles(corpus, block.ToolInput)))
			case "report_documentation_findings":
				toolResults = append(toolResults, toolResult(block.ToolUseID, reportFindings(record, block.ToolInput)))
			case "finish_documentation_review":
				if finishIndex >= 0 {
					toolResults = append(toolResults, toolResult(block.ToolUseID, "rejected: finish_documentation_review was already called in this turn"))
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
			// A review that never opened the project never asked the second
			// question, and "nothing was falsified" is exactly what that looks
			// like from the outside. That confusion is the whole reason this
			// judgment exists, so it must not be reachable.
			if lookups == 0 {
				toolResults[finishIndex] = toolResult(finishID, "rejected: nothing in the project has been searched or read yet, so the question of what this change made untrue has not been asked. Use search_project and read_file on the documents that make claims about what changed, then finish.")
			} else {
				finish = true
				toolResults[finishIndex] = toolResult(finishID, "documentation review recorded")
			}
		}
		if len(toolResults) == 0 {
			return Result{}, errors.New("model neither looked at the project, reported a finding, nor finished the documentation review")
		}
		messages = append(messages, llm.Message{Role: "user", Content: toolResults})
		if finish {
			return Result{
				Kind:            Reviewed,
				Verdict:         DeriveVerdict(record.Findings),
				Findings:        record.Findings,
				Corpus:          corpus.Summary(),
				Lookups:         lookups,
				RequestProvided: requestProvided,
			}, nil
		}
	}
	return Result{}, fmt.Errorf("model did not finish the documentation review within %d turns", maxAgentTurns)
}

func searchProject(corpus *Corpus, input json.RawMessage) string {
	var request struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(input, &request); err != nil {
		return "rejected: decoding search_project call: " + err.Error()
	}
	return corpus.Search(request.Query)
}

func readFile(corpus *Corpus, input json.RawMessage) string {
	var request struct {
		Path     string `json:"path"`
		FromLine int    `json:"from_line"`
	}
	if err := json.Unmarshal(input, &request); err != nil {
		return "rejected: decoding read_file call: " + err.Error()
	}
	return corpus.Read(request.Path, request.FromLine)
}

func listFiles(corpus *Corpus, input json.RawMessage) string {
	var request struct {
		Prefix string `json:"prefix"`
	}
	if err := json.Unmarshal(input, &request); err != nil {
		return "rejected: decoding list_files call: " + err.Error()
	}
	return corpus.List(request.Prefix)
}

// reportedFinding is the tool call's own wire shape, kept separate from
// Finding so the model's field names never decide the report's, and so
// InChange - inspector's fact, not the model's - has no wire form the model
// could set.
type reportedFinding struct {
	Kind                FindingKind `json:"kind"`
	Path                string      `json:"path"`
	Line                int         `json:"line"`
	Claim               string      `json:"claim"`
	Summary             string      `json:"summary"`
	Correction          string      `json:"correction"`
	NeedsClientJudgment bool        `json:"needs_client_judgment"`
	JudgmentReason      string      `json:"judgment_reason"`
}

func reportFindings(record *Record, input json.RawMessage) string {
	var request struct {
		Findings []reportedFinding `json:"findings"`
	}
	if err := json.Unmarshal(input, &request); err != nil {
		return "rejected: decoding report_documentation_findings call: " + err.Error()
	}
	findings := make([]Finding, 0, len(request.Findings))
	for _, reported := range request.Findings {
		findings = append(findings, Finding{
			Kind:                reported.Kind,
			Path:                reported.Path,
			Line:                reported.Line,
			Claim:               reported.Claim,
			Summary:             reported.Summary,
			Correction:          reported.Correction,
			NeedsClientJudgment: reported.NeedsClientJudgment,
			JudgmentReason:      reported.JudgmentReason,
		})
	}
	if err := record.AddFindings(findings); err != nil {
		return "rejected: " + err.Error()
	}
	return fmt.Sprintf("recorded %d documentation finding(s)", len(findings))
}

const agentSystemPrompt = `You are Inspector's documentation judgment. You read a change and the project's own text, and you ask two questions about that change - not one.

1. IS THIS CHANGE DESCRIBED? Look at what the change did and check the documents that already cover that ground. This is the question every documentation step asks, and it is the easy one.

2. HAS THIS CHANGE MADE SOMETHING ALREADY WRITTEN UNTRUE? This is the one that gets missed, because the file that went stale is usually NOT IN THE CHANGE AT ALL. Nothing else in the project ever notices: no test fails because a document lies. Examples of what you are hunting:
   - a list that enumerates things and no longer lists all of them
   - a document promising behavior that moved somewhere else
   - a described default, limit, or exit code that changed
   - an example command, sample configuration, or snippet that no longer runs or no longer matches the real flags
   - a doc comment or inline comment describing how a function behaves after that behavior changed

HOW TO ASK THE SECOND QUESTION. The change tells you what to search for, never where to look. Work out every name the change introduced, renamed, removed, or altered - functions, flags, environment variables, commands, paths, defaults, status strings, limits, error messages - and for each one search the project for it. Then do the thing that catches the hard cases: search for the names of its SIBLINGS, the things it now sits beside. A document that lists four of something is only wrong once there are six, and it does not mention the two new ones, so searching for the new names alone finds nothing. Read every file your searches point at before judging it.

DOCUMENTATION MEANS ANYTHING A PERSON READS AS A CLAIM ABOUT BEHAVIOR, wherever it lives: prose files, README and guide text, doc comments, inline comments, sample configuration, and example commands in scripts or tests. It is not limited to files ending in .md.

REPORTING. Call report_documentation_findings for everything you find, as many times as you like, including zero times if the documentation still describes reality - that is a real result, not a failure. Every finding needs:
   - kind: "falsified_claim" for question 2, "undescribed_change" for question 1.
   - path and line: where the claim is, using the path exactly as the corpus has it and the line number read_file showed you.
   - claim: the text VERBATIM as the file has it. Inspector checks this against the file and rejects the finding if it is not there, so quote, never paraphrase.
   - summary: what the change did that made this claim untrue, or what the document fails to describe.
   - exactly one of:
       correction: the replacement text for that claim, written so it is true of the project as it now stands.
       needs_client_judgment plus judgment_reason: use this when the claim looks falsified but correcting it would need a product decision or a guess at what was intended. Name the claim and why it looks falsified, and do not guess. Never stay silent about one of these.

TWO LIMITS YOU DO NOT CROSS. You report on documentation that already exists; you never propose that the project add a document it has never had, because that is a judgment about the product and it belongs to the Client - inspector rejects a finding naming a file that is not in the corpus. And you never edit anything: a correction is text somebody else applies.

When you are done, call finish_documentation_review. You cannot finish before you have searched or read something in the project, because until then the second question has not been asked.`

// requestGivenDuty and noRequestDuty are the two halves of SPEC.md section
// 4's second limit: the request says what was meant, and where there is no
// request there is no account of intent to work from.
const requestGivenDuty = `REQUEST is above. It is the only account of what was meant. Where a claim needs describing in terms of intent, the request is what you describe; you do not read intent out of the implementation, out of how the change was written, or out of what the author appears to have been trying to do.`

const noRequestDuty = `No request text was given, so you have no account of what was meant. Judge only what you can establish from the change and the project's own text. Any claim whose correction would require knowing the intent behind the change must be reported with needs_client_judgment rather than guessed at.`

var agentTools = []llm.Tool{
	{
		Name:        "search_project",
		Description: "Search the project's documentation corpus for a literal, case-insensitive substring and return matching path:line: text. This is how you find claims about what changed in files the change never touched.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "literal text to look for - a name, flag, default value, or command. Not a regular expression."},
		}, "required": []string{"query"}},
	},
	{
		Name:        "read_file",
		Description: "Read one file from the documentation corpus, annotated with the line numbers a finding must cite.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"path":      map[string]any{"type": "string", "description": "the path exactly as the corpus has it"},
			"from_line": map[string]any{"type": "integer", "description": "line to start at; omit for the start of the file"},
		}, "required": []string{"path"}},
	},
	{
		Name:        "list_files",
		Description: "List documentation corpus paths beginning with a prefix. Omit the prefix to list from the top.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"prefix": map[string]any{"type": "string"},
		}},
	},
	{
		Name:        "report_documentation_findings",
		Description: "Report one or more documentation problems this change caused, each quoting the claim verbatim and either proposing its correction or flagging that it needs the Client's judgment.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"findings": map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "object", "properties": map[string]any{
				"kind":                  map[string]any{"type": "string", "enum": []string{string(FalsifiedClaim), string(UndescribedChange)}},
				"path":                  map[string]any{"type": "string"},
				"line":                  map[string]any{"type": "integer"},
				"claim":                 map[string]any{"type": "string", "description": "the claim verbatim as the file has it; inspector checks it against the file"},
				"summary":               map[string]any{"type": "string"},
				"correction":            map[string]any{"type": "string", "description": "replacement text for the claim; omit only when needs_client_judgment is set"},
				"needs_client_judgment": map[string]any{"type": "boolean"},
				"judgment_reason":       map[string]any{"type": "string", "description": "why this cannot be corrected without the Client; required when needs_client_judgment is set"},
			}, "required": []string{"kind", "path", "line", "claim", "summary"}}},
		}, "required": []string{"findings"}},
	},
	{
		Name:        "finish_documentation_review",
		Description: "Finish the documentation review. Call this once both questions have been asked and every finding reported, including when there are none.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	},
}

func toolResult(id, content string) llm.Block {
	return llm.Block{Type: llm.ToolResult, ToolUseID: id, ToolResult: content}
}
