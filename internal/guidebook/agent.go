package guidebook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/inactdev/inspector/internal/documenter"
	"github.com/inactdev/inspector/internal/llm"
)

// maxAgentTurns is higher than the documenter's: this run writes a whole
// document rather than reporting findings, and it has to read the project's
// entry points, routes and run scripts before it can describe any of them.
const maxAgentTurns = 40

// AgentOptions configures one guidebook generation.
type AgentOptions struct {
	// RepoPath is any path inside the project to describe. Inspector resolves
	// its working-tree root and reads the tracked files there.
	RepoPath   string
	Vendor     string // raw --vendor flag value; empty selects Anthropic.
	Model      string
	APIBaseURL string // test-only Anthropic override; inspector guidebook never accepts one.
}

// Prepare reads the project and extracts its facts, with no model involved and
// no credential needed. It is the grounding half of a generation, exported so
// it can be run and inspected on its own: the facts are the part of the output
// a reader is meant to check, so being able to see them without spending a
// model call is worth a flag.
func Prepare(repoPath string) (*documenter.Corpus, Facts, error) {
	corpus, err := documenter.LoadCorpus(repoPath)
	if err != nil {
		if errors.Is(err, documenter.ErrNoCorpus) {
			return nil, Facts{}, fmt.Errorf("found no readable tracked text file in %s, so there is nothing to learn how to operate this project from; a guidebook written over an empty project would be invention", repoPath)
		}
		return nil, Facts{}, err
	}
	return corpus, ExtractFacts(corpus, headCommit(corpus.Root())), nil
}

// RunAgent generates one project's guidebook: inspector extracts the facts, the
// model reads the project and proposes the prose, and inspector validates every
// piece of it and renders the document itself.
func RunAgent(ctx context.Context, opts AgentOptions) (Result, []byte, error) {
	if strings.TrimSpace(opts.Model) == "" {
		return Result{}, nil, errors.New("no model was configured")
	}
	adapter, _, err := llm.Resolve(opts.Vendor, opts.Model)
	if err != nil {
		return Result{}, nil, err
	}
	if anthropic, ok := adapter.(*llm.AnthropicAdapter); ok && opts.APIBaseURL != "" {
		anthropic.BaseURL = opts.APIBaseURL
	}
	corpus, facts, err := Prepare(opts.RepoPath)
	if err != nil {
		return Result{}, nil, err
	}

	record := NewRecord(corpus, facts)
	var initial strings.Builder
	fmt.Fprintf(&initial, "PROJECT: %s\n\n", facts.Project)
	initial.WriteString(facts.Describe())
	initial.WriteString(corpus.Describe())
	messages := []llm.Message{{Role: "user", Content: []llm.Block{{Type: llm.Text, Text: initial.String()}}}}

	lookups := 0
	for turn := 0; turn < maxAgentTurns; turn++ {
		blocks, err := adapter.Ask(ctx, agentSystemPrompt, agentTools, messages)
		if err != nil {
			return Result{}, nil, err
		}
		if len(blocks) == 0 {
			return Result{}, nil, errors.New("model returned no content")
		}
		messages = append(messages, llm.Message{Role: "assistant", Content: blocks})

		toolResults := make([]llm.Block, 0, len(blocks))
		// finish is answered after every other call in the same turn, so a
		// section or feature proposed alongside it still counts - but its result
		// stays where the model put it, since each tool_use needs its
		// tool_result back in the order it was made.
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
			case "write_sections":
				toolResults = append(toolResults, toolResult(block.ToolUseID, writeSections(record, block.ToolInput)))
			case "write_features":
				toolResults = append(toolResults, toolResult(block.ToolUseID, writeFeatures(record, block.ToolInput)))
			case "finish_guidebook":
				if finishIndex >= 0 {
					toolResults = append(toolResults, toolResult(block.ToolUseID, "rejected: finish_guidebook was already called in this turn"))
					continue
				}
				finishIndex, finishID = len(toolResults), block.ToolUseID
				toolResults = append(toolResults, llm.Block{})
			default:
				toolResults = append(toolResults, toolResult(block.ToolUseID, "tool is not available"))
			}
		}
		var book Guidebook
		finish := false
		if finishIndex >= 0 {
			switch {
			case lookups == 0:
				// The extracted facts are a starting point, not the project. A
				// guidebook written without opening a single file is the
				// template this design rejected, produced at model prices.
				toolResults[finishIndex] = toolResult(finishID, "rejected: nothing in the project has been read yet. The extracted facts say what exists; they do not say what the app is for or how a person uses it. Read the entry points, route handlers and run scripts, then finish.")
			default:
				built, err := record.Build(llm.Describe(opts.Vendor), opts.Model)
				if err != nil {
					toolResults[finishIndex] = toolResult(finishID, "rejected: "+err.Error())
					break
				}
				book, finish = built, true
				toolResults[finishIndex] = toolResult(finishID, "guidebook recorded")
			}
		}
		if len(toolResults) == 0 {
			return Result{}, nil, errors.New("model neither read the project, wrote a section or feature, nor finished the guidebook")
		}
		messages = append(messages, llm.Message{Role: "user", Content: toolResults})
		if finish {
			return Result{
				Kind:     Generated,
				Project:  facts.Project,
				Commit:   facts.Commit,
				Model:    opts.Model,
				Features: book.FeatureNames(),
				Facts:    len(facts.Facts),
				FactsBy:  facts.Categories(),
				FactsCap: facts.Truncated,
				Corpus:   corpus.Summary(),
				Lookups:  lookups,
			}, Render(book), nil
		}
	}
	return Result{}, nil, fmt.Errorf("model did not finish the guidebook within %d turns", maxAgentTurns)
}

func searchProject(corpus *documenter.Corpus, input json.RawMessage) string {
	var request struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(input, &request); err != nil {
		return "rejected: decoding search_project call: " + err.Error()
	}
	return corpus.Search(request.Query)
}

func readFile(corpus *documenter.Corpus, input json.RawMessage) string {
	var request struct {
		Path     string `json:"path"`
		FromLine int    `json:"from_line"`
	}
	if err := json.Unmarshal(input, &request); err != nil {
		return "rejected: decoding read_file call: " + err.Error()
	}
	return corpus.Read(request.Path, request.FromLine)
}

func listFiles(corpus *documenter.Corpus, input json.RawMessage) string {
	var request struct {
		Prefix string `json:"prefix"`
	}
	if err := json.Unmarshal(input, &request); err != nil {
		return "rejected: decoding list_files call: " + err.Error()
	}
	return corpus.List(request.Prefix)
}

// proposedSection and proposedFeature are the tool calls' own wire shapes, kept
// separate from Section and Feature so the model's field names never decide the
// document's.
type proposedSection struct {
	Name       string   `json:"name"`
	Body       string   `json:"body"`
	GroundedIn []string `json:"groundedIn"`
}

type proposedFeature struct {
	Name         string   `json:"name"`
	SubFeatures  string   `json:"subFeatures"`
	HowToReachIt string   `json:"howToReachIt"`
	DrivingIt    string   `json:"drivingIt"`
	Gotchas      string   `json:"gotchas"`
	GroundedIn   []string `json:"groundedIn"`
}

func writeSections(record *Record, input json.RawMessage) string {
	var request struct {
		Sections []proposedSection `json:"sections"`
	}
	if err := json.Unmarshal(input, &request); err != nil {
		return "rejected: decoding write_sections call: " + err.Error()
	}
	sections := make([]Section, 0, len(request.Sections))
	for _, proposed := range request.Sections {
		sections = append(sections, Section{
			Name:       SectionName(strings.TrimSpace(proposed.Name)),
			Body:       proposed.Body,
			GroundedIn: proposed.GroundedIn,
		})
	}
	if err := record.AddSections(sections); err != nil {
		return "rejected: " + err.Error()
	}
	return fmt.Sprintf("recorded %d section(s)", len(sections))
}

func writeFeatures(record *Record, input json.RawMessage) string {
	var request struct {
		Features []proposedFeature `json:"features"`
	}
	if err := json.Unmarshal(input, &request); err != nil {
		return "rejected: decoding write_features call: " + err.Error()
	}
	features := make([]Feature, 0, len(request.Features))
	for _, proposed := range request.Features {
		features = append(features, Feature{
			Name:         proposed.Name,
			SubFeatures:  proposed.SubFeatures,
			HowToReachIt: proposed.HowToReachIt,
			DrivingIt:    proposed.DrivingIt,
			Gotchas:      proposed.Gotchas,
			GroundedIn:   proposed.GroundedIn,
		})
	}
	if err := record.AddFeatures(features); err != nil {
		return "rejected: " + err.Error()
	}
	return fmt.Sprintf("recorded %d feature(s)", len(features))
}

// headCommit is which commit the facts were read at. A guidebook describes one
// version of a project, and a reader checking a quoted line needs to know
// which. It is best-effort: a project read outside a checkout still gets a
// guidebook, it just cannot say what it was read from.
func headCommit(root string) string {
	cmd := exec.Command("git", "-C", root, "rev-parse", "HEAD")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return ""
	}
	return strings.TrimSpace(stdout.String())
}

const agentSystemPrompt = `You are writing Inspector's examiner guidebook for one project. You can read the project's source. The examiner that will use this document cannot - it gets this file and a running app, nothing else. Every single thing it needs in order to start the app, reach a feature and check the result has to be in what you write, because there is no second chance to look.

WHO YOU ARE WRITING FOR. A competent agent that has never seen this codebase, cannot open it, and will follow your instructions literally. If you write "start the server as usual", it stops. If you write a command with a placeholder you never explain, it stops or invents something. If you name an endpoint that does not exist, it reports a bug that is not there. Assume no knowledge and no ability to check.

WHAT THE SECTIONS ARE FOR. Write all six with write_sections:
- Launch: the real commands from a clean checkout, in order, including whatever isolation the project requires (container, virtualenv, node_modules, a database, a storage directory). Must contain a fenced command block. Say what to do about state: a throwaway directory beats a personal one.
- Doctor: how to tell the app is actually up and serving, before judging anything against it. One concrete command and what its healthy output looks like. Must contain a fenced command block. A dead app that reads as a broken feature is the single most common way an examination goes wrong.
- Drive: how the harness talks to this app at all - base URL and port, authentication if any, content types, the general shape of a call or invocation.
- Evidence: where state lands and what success and failure look like, concretely enough that a verdict rests on something checkable rather than on a feeling. Status codes, output text, files, records.
- Cleanup: how to put the machine back, so a second examination is as safe as the first.
- Cannot be tested here: the limits. Anything a harness with only this document and a running app cannot reach - a native mobile screen, a device microphone, a third-party service, a scheduled job - belongs here, named, so the examiner reports it as could-not-be-tested rather than as a failure. Be specific and be honest; this section is what stops the examiner blaming the app for the harness.

THE FEATURE MAP. Call write_features for every user-facing feature, in batches. A feature is something a person does with this app, in their terms - "capture a thought and have it kept", not "the HTTP handler layer" and not "the storage module". A module list is the failure mode here. Each feature needs all four:
- subFeatures: what else lives inside it.
- howToReachIt: how a user arrives at it - what they open, type or call.
- drivingIt: the concrete call the harness makes. A real endpoint with a real method and a real body, a real command line with real arguments. Not a description of one.
- gotchas: what will mislead a judge who cannot read the code. Required ordering, state left behind, a repeated id that updates rather than inserts, a 200 that means nothing happened, a slow first run.

GROUNDING. Every section and every feature carries groundedIn: the ids of the EXTRACTED FACTS it rests on. Those facts were read out of the project by Inspector and are listed with the exact file and line. Cite the ids you actually used. Inspector rejects an id it did not extract, and rejects a feature or a Launch section that cites none - those are the two places where a wrong instruction costs the examiner its whole run.

GO AND LOOK. The extracted facts tell you what exists; they do not tell you what any of it is for. Use search_project, read_file and list_files to read the entry points, the route handlers, the run scripts, the README and the configuration before you describe them. You cannot finish before you have read something. Where the project's own README or contributing guide already documents the way in, prefer it - it is what the maintainers say works.

HONESTY. You are writing a draft a person will check. Where you are unsure, say so in the text rather than writing a confident command you have not verified; "this is how the code reads, confirm it" is useful and a wrong command is not. Never invent a flag, a port or an endpoint to fill a gap.

When every section and every feature is in, call finish_guidebook. Inspector validates the whole document and renders the file itself.`

var agentTools = []llm.Tool{
	{
		Name:        "search_project",
		Description: "Search the project's tracked text for a literal, case-insensitive substring and return matching path:line: text. This is how you find a route handler, a flag, or the place a port is set.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "literal text to look for - a path, flag, function or command. Not a regular expression."},
		}, "required": []string{"query"}},
	},
	{
		Name:        "read_file",
		Description: "Read one of the project's tracked text files, annotated with line numbers.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"path":      map[string]any{"type": "string", "description": "the path exactly as the project listing has it"},
			"from_line": map[string]any{"type": "integer", "description": "line to start at; omit for the start of the file"},
		}, "required": []string{"path"}},
	},
	{
		Name:        "list_files",
		Description: "List the project's tracked paths beginning with a prefix. Omit the prefix to list from the top.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"prefix": map[string]any{"type": "string"},
		}},
	},
	{
		Name:        "write_sections",
		Description: "Write one or more of the guidebook's top-level sections. All six are required before the guidebook can be finished. Markdown body; fenced code blocks for commands.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"sections": map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "object", "properties": map[string]any{
				"name":       map[string]any{"type": "string", "enum": sectionNameEnum()},
				"body":       map[string]any{"type": "string", "description": "markdown. Launch and Doctor must contain a fenced command block."},
				"groundedIn": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "extracted fact ids this section rests on; required for Launch"},
			}, "required": []string{"name", "body"}}},
		}, "required": []string{"sections"}},
	},
	{
		Name:        "write_features",
		Description: "Add user-facing features to the guidebook's feature map. A feature is something a person does with the app, in their terms - never a module. All four fields are required for every feature.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"features": map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "object", "properties": map[string]any{
				"name":         map[string]any{"type": "string"},
				"subFeatures":  map[string]any{"type": "string", "description": "what else lives inside this feature"},
				"howToReachIt": map[string]any{"type": "string", "description": "how a user arrives at it, in their terms"},
				"drivingIt":    map[string]any{"type": "string", "description": "the concrete call or invocation the harness makes"},
				"gotchas":      map[string]any{"type": "string", "description": "what will mislead a judge who cannot read the code"},
				"groundedIn":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "extracted fact ids this feature rests on; at least one is required"},
			}, "required": []string{"name", "subFeatures", "howToReachIt", "drivingIt", "gotchas", "groundedIn"}}},
		}, "required": []string{"features"}},
	},
	{
		Name:        "finish_guidebook",
		Description: "Finish. Inspector validates every section and feature and renders the document. Rejected if a section is missing, a feature is incomplete, or nothing in the project was read.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	},
}

func sectionNameEnum() []string {
	names := make([]string, 0, len(RequiredSections))
	for _, name := range RequiredSections {
		names = append(names, string(name))
	}
	return names
}

func toolResult(id, content string) llm.Block {
	return llm.Block{Type: llm.ToolResult, ToolUseID: id, ToolResult: content}
}
