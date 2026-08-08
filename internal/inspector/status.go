package inspector

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// StatusContext is the stable name GitHub shows for the commit status
// inspector posts, and the exact string inspector-gate (issue #4) must
// key on to find inspector's result among any other statuses on the same
// commit. Documented in SPEC.md section 7 and README.md - do not rename
// it without updating both.
const StatusContext = "inspector"

// GitHubTokenEnvVar is the environment variable inspector reads its
// GitHub token from. SPEC.md section 12 settled where the token lives:
// v1 posts with the Client's own token rather than a token scoped to
// inspector's own identity. That means a green status only proves an
// account posted it, never which program did - the same trust model as
// any other CI system, and no stronger.
const GitHubTokenEnvVar = "GITHUB_TOKEN"

// defaultStatusAPIBaseURL is GitHub's REST API.
const defaultStatusAPIBaseURL = "https://api.github.com"

// StatusState is the state GitHub records for a commit status. Only
// success and failure are used - a Refused outcome posts no status at
// all, so its absence reads as a failure the same way an unreachable
// API or a missing token does (SPEC.md section 7).
type StatusState string

const (
	StatusSuccess StatusState = "success"
	StatusFailure StatusState = "failure"
)

// PostStatusOptions configures one commit-status post.
type PostStatusOptions struct {
	Owner, Repo, Commit string
	State               StatusState
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
// a missing token, an unreachable API, a non-2xx response - comes back
// as a descriptive error naming the concrete problem, so a caller can
// never mistake a failed post for a successful one and silently treat
// an unrecorded result as a recorded one.
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

	body, err := json.Marshal(struct {
		State       string `json:"state"`
		Context     string `json:"context"`
		Description string `json:"description,omitempty"`
	}{
		State:       string(opts.State),
		Context:     StatusContext,
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

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("posting commit status to %s: %w", base, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("GitHub rejected the commit status (%s): %s", resp.Status, strings.TrimSpace(string(respBody)))
	}
	return nil
}
