package inspector

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// PullRequestOptions configures a pull-request lookup or creation. Head is
// the branch name alone, with no owner prefix - the same name Inspector just
// published to Base's repository, so a lookup and the creation it guards
// always describe the same pull request.
type PullRequestOptions struct {
	Owner, Repo string
	Head, Base  string
	Title, Body string

	// Token authenticates the request. Required.
	Token string
	// APIBaseURL overrides GitHub's API host, for tests. Empty uses the
	// real API.
	APIBaseURL string
}

type pullRequestResponse struct {
	HTMLURL string `json:"html_url"`
}

// FindExistingPullRequest reports the URL of an already-open pull request
// from Head into Base, if one exists. Inspector checks this before it ever
// opens one, so publishing the same green branch twice reports the existing
// pull request instead of opening a second one.
func FindExistingPullRequest(opts PullRequestOptions) (prURL string, found bool, err error) {
	if strings.TrimSpace(opts.Token) == "" {
		return "", false, fmt.Errorf("no GitHub token: set %s to a token with pull-request write access", GitHubTokenEnvVar)
	}
	if opts.Owner == "" || opts.Repo == "" {
		return "", false, fmt.Errorf("no GitHub owner/repo to look up a pull request in")
	}
	if opts.Head == "" || opts.Base == "" {
		return "", false, fmt.Errorf("no head/base branch to look up a pull request for")
	}

	base := opts.APIBaseURL
	if base == "" {
		base = defaultStatusAPIBaseURL
	}
	query := url.Values{}
	query.Set("head", opts.Owner+":"+opts.Head)
	query.Set("base", opts.Base)
	requestURL := fmt.Sprintf("%s/repos/%s/%s/pulls?%s", base, opts.Owner, opts.Repo, query.Encode())

	req, err := http.NewRequest(http.MethodGet, requestURL, nil)
	if err != nil {
		return "", false, fmt.Errorf("building pull request lookup: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+opts.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "inspector")

	resp, err := statusHTTPClient.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("looking up an existing pull request at %s: %w", base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", false, fmt.Errorf("GitHub did not return pull requests for %s/%s (%s): %s", opts.Owner, opts.Repo, resp.Status, strings.TrimSpace(string(respBody)))
	}
	var pulls []pullRequestResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, repositoryResponseBodyLimit)).Decode(&pulls); err != nil {
		return "", false, fmt.Errorf("decoding GitHub pull request list: %w", err)
	}
	if len(pulls) == 0 {
		return "", false, nil
	}
	if pulls[0].HTMLURL == "" {
		return "", false, fmt.Errorf("GitHub returned a pull request with no URL for %s/%s", opts.Owner, opts.Repo)
	}
	return pulls[0].HTMLURL, true, nil
}

// OpenPullRequest creates a pull request from Head into Base and returns its
// URL. Callers must call FindExistingPullRequest first - Inspector never
// opens a second pull request for a branch that already has one.
func OpenPullRequest(opts PullRequestOptions) (string, error) {
	if strings.TrimSpace(opts.Token) == "" {
		return "", fmt.Errorf("no GitHub token: set %s to a token with pull-request write access", GitHubTokenEnvVar)
	}
	if opts.Owner == "" || opts.Repo == "" {
		return "", fmt.Errorf("no GitHub owner/repo to open a pull request in")
	}
	if opts.Head == "" || opts.Base == "" {
		return "", fmt.Errorf("no head/base branch to open a pull request for")
	}
	if strings.TrimSpace(opts.Title) == "" {
		return "", fmt.Errorf("no pull request title")
	}

	base := opts.APIBaseURL
	if base == "" {
		base = defaultStatusAPIBaseURL
	}
	body, err := json.Marshal(struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body"`
	}{Title: opts.Title, Head: opts.Head, Base: opts.Base, Body: opts.Body})
	if err != nil {
		return "", fmt.Errorf("encoding pull request body: %w", err)
	}

	requestURL := fmt.Sprintf("%s/repos/%s/%s/pulls", base, opts.Owner, opts.Repo)
	req, err := http.NewRequest(http.MethodPost, requestURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("building pull request creation: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+opts.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "inspector")

	resp, err := statusHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("opening a pull request at %s: %w", base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("GitHub did not create the pull request (%s): %s", resp.Status, strings.TrimSpace(string(respBody)))
	}
	var pr pullRequestResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, repositoryResponseBodyLimit)).Decode(&pr); err != nil {
		return "", fmt.Errorf("decoding GitHub pull request response: %w", err)
	}
	if pr.HTMLURL == "" {
		return "", fmt.Errorf("GitHub created a pull request for %s/%s with no URL", opts.Owner, opts.Repo)
	}
	return pr.HTMLURL, nil
}
