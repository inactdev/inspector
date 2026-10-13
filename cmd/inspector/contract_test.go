package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/inactdev/inspector/internal/inspector"
)

// fabricaConsumerContract mirrors testdata/fabrica-consumer-contract.json:
// Fabrica's documented understanding of how it calls inspector today
// (issue #38). checkFabricaConsumerContract verifies each item against
// inspector's actual behavior and names any item it no longer honors.
type fabricaConsumerContract struct {
	Flags []struct {
		Name string `json:"name"`
	} `json:"flags"`
	ExitCodes map[string]int `json:"exitCodes"`
	Stdout    struct {
		PullRequestLine string `json:"pullRequestLine"`
	} `json:"stdout"`
	Env struct {
		GithubToken string `json:"githubToken"`
	} `json:"env"`
}

func loadFabricaConsumerContract(t *testing.T, path string) fabricaConsumerContract {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var contract fabricaConsumerContract
	if err := json.Unmarshal(data, &contract); err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
	return contract
}

// checkFabricaConsumerContract returns one violation per contract item
// inspector no longer honors, naming it by its contract value, or nil if
// the contract still holds.
func checkFabricaConsumerContract(t *testing.T, contract fabricaConsumerContract) []string {
	t.Helper()
	var violations []string
	violations = append(violations, checkContractFlags(contract.Flags)...)
	violations = append(violations, checkContractExitCodes(contract.ExitCodes)...)
	violations = append(violations, checkContractPullRequestLine(t, contract.Stdout.PullRequestLine)...)
	violations = append(violations, checkContractEnv(contract.Env.GithubToken)...)
	return violations
}

// checkContractFlags parses the real flag set via run()'s own -help output,
// rather than scanning main.go's source, so a rename is caught by actual
// CLI behavior.
func checkContractFlags(flags []struct {
	Name string `json:"name"`
}) []string {
	var stdout, stderr bytes.Buffer
	run([]string{"-help"}, &stdout, &stderr)
	usage := stderr.String()

	var violations []string
	for _, f := range flags {
		pattern := regexp.MustCompile(`(?m)^  -` + regexp.QuoteMeta(f.Name) + `\b`)
		if !pattern.MatchString(usage) {
			violations = append(violations, fmt.Sprintf("flag -%s is no longer accepted", f.Name))
		}
	}
	return violations
}

func checkContractExitCodes(exitCodes map[string]int) []string {
	actual := map[string]int{"green": exitGreen, "red": exitRed, "refused": exitRefused}
	var violations []string
	for name, want := range exitCodes {
		got, known := actual[name]
		if !known {
			violations = append(violations, fmt.Sprintf("exit code name %q is not a verdict inspector reports", name))
			continue
		}
		if got != want {
			violations = append(violations, fmt.Sprintf("exit code for %q is now %d, contract says %d", name, got, want))
		}
	}
	return violations
}

func checkContractEnv(githubToken string) []string {
	if githubToken != inspector.GitHubTokenEnvVar {
		return []string{fmt.Sprintf("GitHub token environment variable is now %q, contract says %q", inspector.GitHubTokenEnvVar, githubToken)}
	}
	return nil
}

// checkContractPullRequestLine drives openGreenPullRequest - the same
// function runCLI exercises for a green result with -pr-title/-pr-body-file
// - against a stub GitHub API, without needing Docker or a real git remote.
func checkContractPullRequestLine(t *testing.T, template string) []string {
	t.Helper()
	prefix, _, ok := strings.Cut(template, "<url>")
	if !ok || prefix == "" {
		t.Fatalf("contract stdout.pullRequestLine %q has no <url> placeholder", template)
	}

	pulls := &fakePullRequests{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
			pulls.handleList(w, r)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			pulls.handleCreate(w, r)
		case r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
		}
	}))
	defer server.Close()

	prevBaseURL := githubAPIBaseURL
	githubAPIBaseURL = server.URL
	defer func() { githubAPIBaseURL = prevBaseURL }()
	t.Setenv(inspector.GitHubTokenEnvVar, "test-token")

	snapshot := publicationTargetSnapshot{
		target: inspector.PublicationTarget{Owner: "acme", Repo: "demo"},
	}
	var stdout, stderr bytes.Buffer
	openGreenPullRequest(snapshot, "feature", "Implement login", "the login flow is done", inspector.Result{}, &stdout, &stderr)

	if len(pulls.Created) != 1 {
		t.Fatalf("test setup: openGreenPullRequest did not create exactly one pull request (created=%d, stderr=%q)", len(pulls.Created), stderr.String())
	}
	wantLine := prefix + pulls.Created[0].URL
	if !strings.Contains(stdout.String(), wantLine) {
		return []string{fmt.Sprintf("stdout no longer contains %q after opening a pull request (got %q)", wantLine, stdout.String())}
	}
	return nil
}

func TestFabricaConsumerContract(t *testing.T) {
	contract := loadFabricaConsumerContract(t, "testdata/fabrica-consumer-contract.json")
	if violations := checkFabricaConsumerContract(t, contract); len(violations) > 0 {
		t.Fatalf("inspector no longer honors Fabrica's consumer contract:\n%s", strings.Join(violations, "\n"))
	}
}

// TestFabricaConsumerContract_CatchesRenamedFlag is the negative test: a
// copy of the contract with -pr-title renamed to -pr-title-renamed must
// fail, naming that flag, proving the positive test above would actually
// catch a real break instead of trivially passing.
func TestFabricaConsumerContract_CatchesRenamedFlag(t *testing.T) {
	contract := loadFabricaConsumerContract(t, "testdata/fabrica-consumer-contract.renamed-flag.json")
	violations := checkFabricaConsumerContract(t, contract)
	if len(violations) == 0 {
		t.Fatal("expected the renamed flag to be reported as a violation, got none")
	}
	want := "flag -pr-title-renamed is no longer accepted"
	for _, v := range violations {
		if v == want {
			return
		}
	}
	t.Fatalf("violations = %v, want one of them to be %q", violations, want)
}
