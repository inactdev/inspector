package inspector

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"time"
)

// StatusContext is the stable name GitHub shows for the commit status
// inspector posts, and the exact string inspector-gate (issue #4) must
// key on to find inspector's result among any other statuses on the same
// commit. Documented in SPEC.md section 7 and README.md, and spelled out
// a third time as the gate workflow's own STATUS_CONTEXT env value in
// .github/workflows/inspector-gate.yml - do not rename it without
// updating all three, or the gate stops finding this result.
const StatusContext = "inspector"

// ExaminerStatusContext is the separate status the independent examiner posts
// after judging request-derived outcomes.
const ExaminerStatusContext = "examiner"

// GitHubTokenEnvVar is the environment variable inspector reads its
// GitHub token from. SPEC.md section 12 settled where the token lives:
// v1 posts with the Client's own token rather than a token scoped to
// inspector's own identity. That means a green status only proves an
// account posted it, never which program did - the same trust model as
// any other CI system, and no stronger.
const GitHubTokenEnvVar = "GITHUB_TOKEN"

// defaultStatusAPIBaseURL is GitHub's REST API.
const defaultStatusAPIBaseURL = "https://api.github.com"

const repositoryResponseBodyLimit = 1 << 20

// statusPostTimeout bounds the whole post. inspector runs unattended
// (SPEC.md section 3), so a blackholed api.github.com must become a loud
// failure rather than a hang that outlives the check timeout the verdict
// was already produced under.
const statusPostTimeout = 30 * time.Second

// statusHTTPClient refuses to follow redirects. Go turns a redirected
// POST into a GET, and GitHub answers 301 for a renamed or transferred
// repository - the redirected GET on the same path is the valid "list
// commit statuses" call, which answers 200 having recorded nothing. Left
// followed, that reads as a successful post and an unrecorded result
// looks like success, which SPEC.md section 7 forbids.
var statusHTTPClient = &http.Client{
	Timeout: statusPostTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// StatusState is the state GitHub records for a commit status. The project
// check publishes only success under StatusContext. The examiner uses success,
// failure, or error under ExaminerStatusContext so its judgment and refusals
// remain separate and visible.
type StatusState string

const (
	StatusSuccess StatusState = "success"
	StatusFailure StatusState = "failure"
	StatusError   StatusState = "error"
)

// RepositoryOptions identifies a GitHub repository.
type RepositoryOptions struct {
	Owner, Repo string
	Token       string
	APIBaseURL  string
}

// RepositoryDefaultBranch returns the repository's configured default branch.
func RepositoryDefaultBranch(opts RepositoryOptions) (string, error) {
	if strings.TrimSpace(opts.Token) == "" {
		return "", fmt.Errorf("no GitHub token: set %s to read the repository default branch", GitHubTokenEnvVar)
	}
	if opts.Owner == "" || opts.Repo == "" {
		return "", fmt.Errorf("no GitHub owner/repo to read the default branch from")
	}

	base := opts.APIBaseURL
	if base == "" {
		base = defaultStatusAPIBaseURL
	}
	url := fmt.Sprintf("%s/repos/%s/%s", base, opts.Owner, opts.Repo)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("building repository request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+opts.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "inspector")

	resp, err := statusHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("reading the default branch from %s: %w", base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("GitHub did not return the repository default branch (%s): %s", resp.Status, strings.TrimSpace(string(respBody)))
	}
	var repository struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, repositoryResponseBodyLimit)).Decode(&repository); err != nil {
		return "", fmt.Errorf("decoding GitHub repository response: %w", err)
	}
	if repository.DefaultBranch == "" {
		return "", fmt.Errorf("GitHub returned no default branch for %s/%s", opts.Owner, opts.Repo)
	}
	return repository.DefaultBranch, nil
}

// PostStatusOptions configures one commit-status post.
type PostStatusOptions struct {
	Owner, Repo, Commit string
	State               StatusState
	// Context defaults to StatusContext for the existing project-check status.
	// The examiner supplies ExaminerStatusContext so GitHub keeps the two
	// independent judgments separate.
	Context string
	// Description is shown next to the status on GitHub. It is not
	// where the detail lives - SPEC.md keeps the status tiny and the
	// per-run report (report.go) as the place a reader goes for what
	// actually happened.
	Description string

	// Token authenticates the request. Required - PostCommitStatus
	// fails loudly rather than posting unauthenticated.
	Token string
	// APIBaseURL overrides GitHub's API host, for tests. Empty uses the
	// real API.
	APIBaseURL string
}

// PostCommitStatus posts a commit status to GitHub's REST API
// (https://docs.github.com/en/rest/commits/statuses). Every failure -
// a missing token, an unreachable API, anything but the documented 201
// Created (a redirect included) - comes back
// as a descriptive error naming the concrete problem. Server failures
// after the request was sent are reported as unconfirmed because an
// intermediary may have lost GitHub's successful response.
func PostCommitStatus(opts PostStatusOptions) error {
	if strings.TrimSpace(opts.Token) == "" {
		return fmt.Errorf("no GitHub token: set %s to a token with commit-status write access", GitHubTokenEnvVar)
	}
	if opts.Owner == "" || opts.Repo == "" {
		return fmt.Errorf("no GitHub owner/repo to post a status to")
	}
	if opts.Commit == "" {
		return fmt.Errorf("no commit to post a status to")
	}

	base := opts.APIBaseURL
	if base == "" {
		base = defaultStatusAPIBaseURL
	}

	contextName := opts.Context
	if contextName == "" {
		contextName = StatusContext
	}
	body, err := json.Marshal(struct {
		State       string `json:"state"`
		Context     string `json:"context"`
		Description string `json:"description,omitempty"`
	}{
		State:       string(opts.State),
		Context:     contextName,
		Description: opts.Description,
	})
	if err != nil {
		return fmt.Errorf("encoding status body: %w", err)
	}

	url := fmt.Sprintf("%s/repos/%s/%s/statuses/%s", base, opts.Owner, opts.Repo, opts.Commit)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building status request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+opts.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "inspector")

	var requestWritten atomic.Bool
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				requestWritten.Store(true)
			}
		},
	}))
	resp, err := statusHTTPClient.Do(req)
	if err != nil {
		if requestWritten.Load() {
			return fmt.Errorf("posting commit status to %s failed after the request was written: stamp sent, outcome unconfirmed: %w", base, err)
		}
		return fmt.Errorf("status stamp not sent; posting commit status to %s: %w", base, err)
	}
	defer resp.Body.Close()

	// Only the documented 201 Created confirms that GitHub recorded the
	// status. A server failure after the request was written is ambiguous;
	// explicit client rejections and stopped redirects are definitive.
	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		detail := strings.TrimSpace(string(respBody))
		if commitUnknownToGitHub(resp.StatusCode, detail) {
			return fmt.Errorf("GitHub has no commit %s in %s/%s (%s): inspector must first make a green commit available through its temporary staging ref before recording the status - a commit status can only attach to a commit GitHub already has: %s",
				opts.Commit, opts.Owner, opts.Repo, resp.Status, detail)
		}
		if resp.StatusCode >= http.StatusInternalServerError {
			return fmt.Errorf("GitHub returned %s after the status request was written: stamp sent, outcome unconfirmed: %s", resp.Status, detail)
		}
		if loc := resp.Header.Get("Location"); loc != "" {
			detail = strings.TrimSpace(fmt.Sprintf("redirected to %s (the repository may have been renamed or transferred; update the 'origin' remote) %s", loc, detail))
		}
		return fmt.Errorf("GitHub did not record the commit status (%s): %s", resp.Status, detail)
	}
	return nil
}

// commitUnknownToGitHub reports whether a rejection means GitHub has
// never seen this commit, which it answers with "No commit found for
// SHA: ..." (422 for a well-formed SHA it does not have). Inspector
// normally stages a green commit before posting, so this response means
// that staging did not make the checked commit available. The body is
// accurate but names no cause, so the error explains inspector's own
// required order rather than telling a builder to push the commit.
func commitUnknownToGitHub(statusCode int, body string) bool {
	if statusCode != http.StatusUnprocessableEntity && statusCode != http.StatusNotFound {
		return false
	}
	return strings.Contains(strings.ToLower(body), "no commit found")
}
