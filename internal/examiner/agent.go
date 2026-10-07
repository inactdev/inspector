package examiner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	AnthropicAPIKeyEnvVar = "ANTHROPIC_API_KEY"
	defaultAnthropicURL   = "https://api.anthropic.com/v1/messages"
	maxAppResponseBytes   = 64 << 10
	maxAgentTurns         = 32
)

// AgentOptions configures the sealed examiner process. Its input directory
// contains only PreparedInputs files and its output directory is disposable.
type AgentOptions struct {
	InputDir   string
	OutputDir  string
	AppURL     string
	Model      string
	APIBaseURL string // test-only override; inspector examine never accepts one.
	Budget     int
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

// RunAgent asks the model to propose scenarios and operate the running app
// through a constrained HTTP driver. The model never submits a verdict. The
// machine records delivered observations per capability and derives the result.
func RunAgent(ctx context.Context, opts AgentOptions) (runErr error) {
	if strings.TrimSpace(opts.Model) == "" {
		return errors.New("no model was configured")
	}
	baseURL, err := validAppURL(opts.AppURL)
	if err != nil {
		return err
	}
	request, err := readAgentFile(opts.InputDir, InputRequestName)
	if err != nil {
		return err
	}
	featureMap, err := readAgentFile(opts.InputDir, InputFeatureMapName)
	if err != nil {
		return err
	}
	alwaysTrue, err := readAgentFile(opts.InputDir, InputAlwaysTrueName)
	if err != nil {
		return err
	}
	changedFilesData, err := readAgentFile(opts.InputDir, InputChangedFilesName)
	if err != nil {
		return err
	}
	changedFiles, err := ParseChangedFiles(changedFilesData)
	if err != nil {
		return err
	}
	baseTestsData, err := readAgentFile(opts.InputDir, InputBaseTestsName)
	if err != nil {
		return err
	}
	baseTests, err := ParseBaseTests(baseTestsData, changedFiles)
	if err != nil {
		return err
	}
	if baseTests.BaseCommit != changedFiles.BaseCommit {
		return fmt.Errorf("base-test list baseCommit does not match changed-file list")
	}
	if _, err := os.Stat(opts.OutputDir); err != nil {
		return fmt.Errorf("examiner output directory: %w", err)
	}

	initial := fmt.Sprintf("REQUEST (derive the requested capabilities from this, never a worker checklist):\n%s\n\nFEATURE MAP (features and how to drive the running app):\n%s\n\nALWAYS-TRUE LIST (project invariants):\n%s\n\nCHANGED FILE NAMES (worker output, names only):\n%s\n\nPRE-TASK CHANGED TESTS (from the task starting commit, never worker content):\n%s", request, featureMap, alwaysTrue, changedFilesData, baseTestsData)
	messages := []anthropicMessage{{Role: "user", Content: initial}}
	record := NewExaminationRecord(opts.Budget)
	defer func() {
		if runErr == nil || len(record.Capabilities) == 0 {
			return
		}
		if err := writeTerminalResult(opts.OutputDir, record, runErr); err != nil {
			runErr = errors.Join(runErr, err)
		}
	}()
	baseTestPaths := make(map[string]struct{}, len(baseTests.Tests))
	for _, test := range baseTests.Tests {
		baseTestPaths[test.Path] = struct{}{}
	}

	for turn := 0; turn < maxAgentTurns; turn++ {
		response, err := askModel(ctx, opts, messages)
		if err != nil {
			return err
		}
		if len(response.Content) == 0 {
			return errors.New("model returned no content")
		}
		messages = append(messages, anthropicMessage{Role: "assistant", Content: response.Content})

		toolResults := make([]map[string]any, 0, len(response.Content))
		pendingObservations := make([]Observation, 0)
		finishResultIndexes := make([]int, 0)
		operationRejected := false
		finish := false
		for _, block := range response.Content {
			if block.Type != "tool_use" {
				continue
			}
			switch block.Name {
			case "propose_capabilities":
				answer, proposalErr := addCapabilities(record, block.Input, baseTestPaths)
				if proposalErr != nil {
					operationRejected = true
					answer = "proposal rejected: " + proposalErr.Error()
				} else if err := writeDerivedResult(opts.OutputDir, record); err != nil {
					return err
				}
				toolResults = append(toolResults, toolResult(block.ID, answer))
			case "drive_app":
				answer, observation, accepted, err := driveCapability(ctx, record, baseURL, block.Input, func() error {
					return writeDerivedResult(opts.OutputDir, record)
				})
				if err != nil {
					return err
				}
				if !accepted {
					operationRejected = true
				}
				if observation != nil {
					pendingObservations = append(pendingObservations, *observation)
				}
				toolResults = append(toolResults, toolResult(block.ID, answer))
			case "assess_capabilities":
				answer, assessmentErr := addAssessments(record, block.Input)
				if assessmentErr != nil {
					operationRejected = true
					answer = "assessment rejected: " + assessmentErr.Error()
				} else if err := writeDerivedResult(opts.OutputDir, record); err != nil {
					return err
				}
				toolResults = append(toolResults, toolResult(block.ID, answer))
			case "finish_examination":
				finish = true
				finishResultIndexes = append(finishResultIndexes, len(toolResults))
				toolResults = append(toolResults, toolResult(block.ID, "the examination record will now derive every outcome"))
			default:
				operationRejected = true
				toolResults = append(toolResults, toolResult(block.ID, "tool is not available"))
			}
		}
		if len(toolResults) == 0 {
			return errors.New("model did not propose capabilities, drive the app, assess evidence, or finish")
		}
		if finish && operationRejected {
			finish = false
			for _, index := range finishResultIndexes {
				toolResults[index]["content"] = "finish rejected because another tool operation in this turn was rejected"
			}
		}
		messages = append(messages, anthropicMessage{Role: "user", Content: toolResults})
		// Observations become available only after their tool result was added
		// to messages, so an assessment in the same model response cannot race
		// a request whose response it has not seen.
		for _, observation := range pendingObservations {
			if err := record.RecordObservation(observation.CapabilityID, observation.Evidence); err != nil {
				return err
			}
			if err := writeDerivedResult(opts.OutputDir, record); err != nil {
				return err
			}
		}
		if finish {
			if err := requireBaseTestScenarios(record, baseTestPaths); err != nil {
				return err
			}
			return writeDerivedResult(opts.OutputDir, record)
		}
	}
	return fmt.Errorf("model did not finish the examination within %d turns", maxAgentTurns)
}

func readAgentFile(dir, name string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil, fmt.Errorf("reading examiner input %s: %w", name, err)
	}
	if len(data) > maxInputBytes {
		return nil, fmt.Errorf("reading examiner input %s: exceeds the %d byte limit", name, maxInputBytes)
	}
	return data, nil
}

func addCapabilities(record *ExaminationRecord, input json.RawMessage, baseTestPaths map[string]struct{}) (string, error) {
	var request struct {
		Capabilities []CapabilityProposal `json:"capabilities"`
	}
	if err := json.Unmarshal(input, &request); err != nil {
		return "", fmt.Errorf("decoding capability proposals: %w", err)
	}
	if len(request.Capabilities) == 0 {
		return "", errors.New("no capabilities were proposed")
	}
	staged := *record
	staged.Capabilities = make(map[string]CapabilityProposal, len(record.Capabilities)+len(request.Capabilities))
	for id, capability := range record.Capabilities {
		staged.Capabilities[id] = capability
	}
	for _, proposal := range request.Capabilities {
		if proposal.TestPath != "" {
			if _, ok := baseTestPaths[proposal.TestPath]; !ok {
				return "", fmt.Errorf("test protection %q has no supplied pre-task test", proposal.TestPath)
			}
		}
		if err := staged.AddCapability(proposal); err != nil {
			return "", err
		}
	}
	record.Capabilities = staged.Capabilities
	return fmt.Sprintf("recorded %d capability proposals", len(request.Capabilities)), nil
}

func driveCapability(ctx context.Context, record *ExaminationRecord, base *url.URL, input json.RawMessage, checkpoint func() error) (string, *Observation, bool, error) {
	var request struct {
		CapabilityID string     `json:"capabilityId"`
		Request      AppRequest `json:"request"`
	}
	if err := json.Unmarshal(input, &request); err != nil {
		return "driver error: decoding drive_app call: " + err.Error(), nil, false, nil
	}
	normalizedRequest := normalizedAppRequest(request.Request)
	if err := record.StartAttempt(request.CapabilityID, normalizedRequest); err != nil {
		return "driver error: " + err.Error(), nil, false, nil
	}
	if err := checkpoint(); err != nil {
		return "", nil, true, err
	}
	answer, err := driveApp(ctx, base, normalizedRequest)
	if err != nil {
		answer = "driver error: " + err.Error()
		if recordErr := record.CompleteAttempt(request.CapabilityID, false, answer); recordErr != nil {
			return "driver error: " + recordErr.Error(), nil, true, nil
		}
		if err := checkpoint(); err != nil {
			return "", nil, true, err
		}
		return answer, nil, true, nil
	}
	if err := record.CompleteAttempt(request.CapabilityID, true, answer); err != nil {
		return "driver error: " + err.Error(), nil, true, nil
	}
	if err := checkpoint(); err != nil {
		return "", nil, true, err
	}
	return answer, &Observation{CapabilityID: request.CapabilityID, Evidence: answer}, true, nil
}

func addAssessments(record *ExaminationRecord, input json.RawMessage) (string, error) {
	var request struct {
		Assessments []Assessment `json:"assessments"`
	}
	if err := json.Unmarshal(input, &request); err != nil {
		return "", fmt.Errorf("decoding assessments: %w", err)
	}
	if len(request.Assessments) == 0 {
		return "", errors.New("no assessments were proposed")
	}
	staged := *record
	staged.Assessments = make(map[string]Assessment, len(record.Assessments)+len(request.Assessments))
	for id, assessment := range record.Assessments {
		staged.Assessments[id] = assessment
	}
	for _, assessment := range request.Assessments {
		if err := staged.AddAssessment(assessment); err != nil {
			return "", err
		}
	}
	record.Assessments = staged.Assessments
	return fmt.Sprintf("recorded %d evidence assessments", len(request.Assessments)), nil
}

func requireBaseTestScenarios(record *ExaminationRecord, baseTestPaths map[string]struct{}) error {
	covered := make(map[string]struct{}, len(baseTestPaths))
	for _, capability := range record.Capabilities {
		if capability.TestPath != "" {
			covered[capability.TestPath] = struct{}{}
		}
	}
	for path := range baseTestPaths {
		if _, ok := covered[path]; !ok {
			return fmt.Errorf("model did not propose a scenario for pre-task test %q", path)
		}
	}
	return nil
}

func writeDerivedResult(outputDir string, record *ExaminationRecord) error {
	result, err := record.DeriveResult()
	if err != nil {
		return fmt.Errorf("deriving examination result: %w", err)
	}
	return writeResult(outputDir, result)
}

func writeTerminalResult(outputDir string, record *ExaminationRecord, terminalErr error) error {
	result, err := record.DeriveResult()
	if err != nil {
		return fmt.Errorf("deriving partial examination result: %w", err)
	}
	markExaminationIncomplete(&result, "examination ended before completion: "+terminalErr.Error())
	return writeResult(outputDir, result)
}

func writeResult(outputDir string, result Result) error {
	result.RuntimeFingerprint = RuntimeSourceFingerprint()
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding verdict: %w", err)
	}
	data = append(data, '\n')
	file, err := os.CreateTemp(outputDir, ".verdict-*.json")
	if err != nil {
		return fmt.Errorf("creating verdict checkpoint: %w", err)
	}
	name := file.Name()
	defer os.Remove(name)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("securing verdict checkpoint: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("writing verdict checkpoint: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("writing verdict checkpoint: %w", err)
	}
	if err := os.Rename(name, filepath.Join(outputDir, VerdictName)); err != nil {
		return fmt.Errorf("publishing verdict checkpoint: %w", err)
	}
	return nil
}

func markExaminationIncomplete(result *Result, message string) {
	result.Verdict.ExaminationIncomplete = true
	result.Message = message
	if result.Kind == Green {
		result.Kind = Refused
	}
}

func askModel(ctx context.Context, opts AgentOptions, messages []anthropicMessage) (anthropicResponse, error) {
	key := os.Getenv(AnthropicAPIKeyEnvVar)
	if strings.TrimSpace(key) == "" {
		return anthropicResponse{}, fmt.Errorf("%s is not set", AnthropicAPIKeyEnvVar)
	}
	body, err := json.Marshal(anthropicMessageRequest{
		Model:     opts.Model,
		MaxTokens: 4096,
		System:    `You are Inspector's independent examiner. Derive observable capabilities from REQUEST first, nearby scenarios from FEATURE MAP and ALWAYS-TRUE LIST within the stated attempt budget, and one test-protection scenario for every entry in PRE-TASK CHANGED TESTS. Changed file names are signals only. Never infer application source. Propose every capability with propose_capabilities before driving it. Call drive_app with that capability id. A successful response will be delivered in a later turn; only then may you assess it with assess_capabilities. You never submit a verdict: after attempts and assessments, call finish_examination and Inspector derives each outcome from the record. For an unresponsive app, still propose every capability, try it, then finish so each becomes could_not_be_tested. A not-confirmed assessment needs a proposed permanent regression line. Never edit project files.`,
		Tools: []anthropicTool{
			{
				Name:        "propose_capabilities",
				Description: "Record request-derived, nearby, invariant, or pre-task-test scenarios before driving them. testPath is only for a supplied pre-task test.",
				InputSchema: map[string]any{"type": "object", "properties": map[string]any{
					"capabilities": map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "object", "properties": map[string]any{
						"id":       map[string]any{"type": "string"},
						"claim":    map[string]any{"type": "string"},
						"scenario": map[string]any{"type": "string"},
						"testPath": map[string]any{"type": "string"},
					}, "required": []string{"id", "claim", "scenario"}}},
				}, "required": []string{"capabilities"}},
			},
			{
				Name:        "drive_app",
				Description: "Make one HTTP request for an already-proposed capability. path must be relative to the configured app URL.",
				InputSchema: map[string]any{"type": "object", "properties": map[string]any{
					"capabilityId": map[string]any{"type": "string"},
					"request": map[string]any{"type": "object", "properties": map[string]any{
						"method":  map[string]any{"type": "string", "enum": []string{"GET", "POST", "PUT", "PATCH", "DELETE"}},
						"path":    map[string]any{"type": "string"},
						"headers": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
						"body":    map[string]any{"type": "string"},
					}, "required": []string{"method", "path"}},
				}, "required": []string{"capabilityId", "request"}},
			},
			{
				Name:        "assess_capabilities",
				Description: "Interpret one or more successful, earlier-turn app observations. Assessment is a proposal; Inspector derives the verdict from its record.",
				InputSchema: map[string]any{"type": "object", "properties": map[string]any{
					"assessments": map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "object", "properties": map[string]any{
						"capabilityId":       map[string]any{"type": "string"},
						"verdict":            map[string]any{"type": "string", "enum": []string{string(Confirmed), string(NotConfirmed)}},
						"evidence":           map[string]any{"type": "string"},
						"proposedRegression": map[string]any{"type": "string"},
					}, "required": []string{"capabilityId", "verdict", "evidence"}}},
				}, "required": []string{"assessments"}},
			},
			{
				Name:        "finish_examination",
				Description: "Finish. Inspector derives every per-capability outcome, findings, and incompleteness from the record.",
				InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			},
		},
		Messages: messages,
	})
	if err != nil {
		return anthropicResponse{}, fmt.Errorf("encoding model request: %w", err)
	}
	endpoint := opts.APIBaseURL
	if endpoint == "" {
		endpoint = defaultAnthropicURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return anthropicResponse{}, fmt.Errorf("building model request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	client := &http.Client{
		Timeout: 60 * time.Second,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			return fmt.Errorf("model API redirected to %s", req.URL.Redacted())
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return anthropicResponse{}, fmt.Errorf("calling model: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxInputBytes+1))
	if err != nil {
		return anthropicResponse{}, fmt.Errorf("reading model response: %w", err)
	}
	if len(data) > maxInputBytes {
		return anthropicResponse{}, errors.New("model response exceeds the examiner input limit")
	}
	if resp.StatusCode != http.StatusOK {
		return anthropicResponse{}, fmt.Errorf("model returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var result anthropicResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return anthropicResponse{}, fmt.Errorf("decoding model response: %w", err)
	}
	return result, nil
}

func toolResult(id, content string) map[string]any {
	return map[string]any{"type": "tool_result", "tool_use_id": id, "content": content}
}

func validAppURL(raw string) (*url.URL, error) {
	base, err := url.Parse(raw)
	if err != nil || base.Scheme == "" || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
		return nil, fmt.Errorf("invalid app URL %q: provide an http or https URL for the already-running app", raw)
	}
	return base, nil
}

func driveApp(ctx context.Context, base *url.URL, call AppRequest) (string, error) {
	call = normalizedAppRequest(call)
	method := call.Method
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return "", fmt.Errorf("unsupported HTTP method %q", call.Method)
	}
	relative, err := url.Parse(call.Path)
	if err != nil || relative.IsAbs() || relative.Host != "" || strings.HasPrefix(call.Path, "//") {
		return "", fmt.Errorf("path %q is not a relative app path", call.Path)
	}
	target := base.ResolveReference(relative)
	if target.Scheme != base.Scheme || target.Host != base.Host {
		return "", fmt.Errorf("path %q escapes the configured app URL", call.Path)
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), strings.NewReader(call.Body))
	if err != nil {
		return "", fmt.Errorf("building app request: %w", err)
	}
	for name, value := range call.Headers {
		req.Header.Set(name, value)
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if req.URL.Scheme != base.Scheme || req.URL.Host != base.Host {
				return fmt.Errorf("redirect escapes the configured app origin to %s", req.URL.Redacted())
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("calling app: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAppResponseBytes+1))
	if err != nil {
		return "", fmt.Errorf("reading app response: %w", err)
	}
	if len(body) > maxAppResponseBytes {
		return "", fmt.Errorf("app response exceeds the %d byte driver limit", maxAppResponseBytes)
	}
	return fmt.Sprintf("HTTP %s\nContent-Type: %s\n\n%s", resp.Status, resp.Header.Get("Content-Type"), body), nil
}
