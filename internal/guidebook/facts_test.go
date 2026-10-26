package guidebook

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inactdev/inspector/internal/documenter"
)

// newRepo writes files into a fresh git repository and commits them, because
// the facts are read from the project's tracked text and nothing else.
func newRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	git(t, dir, "init", "-q")
	git(t, dir, "config", "user.email", "test@example.com")
	git(t, dir, "config", "user.name", "Test")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "initial", "--allow-empty")
	return dir
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func extract(t *testing.T, files map[string]string) Facts {
	t.Helper()
	_, facts, err := Prepare(newRepo(t, files))
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	return facts
}

// has reports whether any fact in the category quotes detail.
func has(facts Facts, category FactCategory, detail string) bool {
	for _, fact := range facts.Facts {
		if fact.Category == category && strings.Contains(fact.Detail, detail) {
			return true
		}
	}
	return false
}

func quoted(facts Facts, category FactCategory) []string {
	var details []string
	for _, fact := range facts.Facts {
		if fact.Category == category {
			details = append(details, fact.Detail)
		}
	}
	return details
}

// webApp is a project of the shape #22 has to work for without ever having seen
// it: a service with a compose file, a Dockerfile, routes in two languages, a
// run script, and an environment it reads.
var webApp = map[string]string{
	".inspector.json": `{"check": "script/check", "image": "cimg/go:1.22"}`,
	"package.json": `{
  "name": "orchard",
  "scripts": {"dev": "vite", "test": "vitest run"},
  "bin": {"orchard": "./cli.js"}
}`,
	"docker-compose.yml": "services:\n  api:\n    image: node:20\n    ports:\n      - \"8080:8080\"\n  db:\n    image: postgres:16\n",
	"Dockerfile":         "FROM node:20\nEXPOSE 8080\nCMD [\"node\", \"server.js\"]\n",
	"server.js": `const app = express()
app.get('/baskets', listBaskets)
app.post('/baskets', createBasket)
const port = process.env.ORCHARD_PORT || "0.0.0.0:8080"
app.listen(port)
`,
	"api/handlers.go": `func register(mux *http.ServeMux) {
	mux.HandleFunc("/health", health)
}
const TokenEnvVar = "ORCHARD_TOKEN"
func token() string { return os.Getenv("ORCHARD_HOME") }
`,
	"api/views.py":           "@app.route(\"/legacy\")\ndef legacy():\n    return \"\"\n",
	"script/check":           "#!/usr/bin/env bash\nnpm test\n",
	"server_test.js":         "app.get('/only-in-a-test', fixture)\n",
	"testdata/sample/app.js": "app.get('/fixture-route', x)\n",
	".env.example":           "ORCHARD_TOKEN=\nORCHARD_PORT=8080\n",
}

func TestExtractFacts_ReadsHowToOperateAProjectItHasNeverSeen(t *testing.T) {
	facts := extract(t, webApp)

	if facts.Project != "orchard" {
		t.Errorf("Project = %q, want the project's own manifest name", facts.Project)
	}
	if facts.Commit == "" {
		t.Error("Commit is empty, want the commit the facts were read at")
	}
	for _, want := range []struct {
		category FactCategory
		detail   string
	}{
		{FactCheckCommand, "script/check"},
		{FactContainer, "cimg/go:1.22"},
		{FactContainer, "EXPOSE 8080"},
		{FactManifest, "npm package orchard"},
		{FactRunScript, "npm run dev -> vite"},
		{FactRunScript, "script/check"},
		{FactService, "api (compose service)"},
		{FactService, "db (compose service)"},
		{FactPort, "8080:8080"},
		{FactRoute, "GET /baskets"},
		{FactRoute, "POST /baskets"},
		{FactRoute, "(any method) /health"},
		{FactRoute, "(any method) /legacy"},
		{FactEntryPoint, "orchard"},
		{FactEnvironment, "ORCHARD_PORT"},
		{FactEnvironment, "ORCHARD_HOME"},
		{FactEnvironment, "ORCHARD_TOKEN"},
		{FactListen, "0.0.0.0:8080"},
	} {
		if !has(facts, want.category, want.detail) {
			t.Errorf("no %s fact quoting %q; facts were %v", want.category, want.detail, quoted(facts, want.category))
		}
	}
}

// TestExtractFacts_EveryFactNamesWhereItCameFrom is what makes the appendix
// checkable: a reader who doubts an instruction goes to the line.
func TestExtractFacts_EveryFactNamesWhereItCameFrom(t *testing.T) {
	facts := extract(t, webApp)
	if len(facts.Facts) == 0 {
		t.Fatal("no facts were extracted at all")
	}
	ids := map[string]struct{}{}
	for _, fact := range facts.Facts {
		if fact.ID == "" || fact.Path == "" || fact.Line < 1 || fact.Detail == "" {
			t.Errorf("fact %#v is missing an id, path, line or detail", fact)
		}
		if _, exists := ids[fact.ID]; exists {
			t.Errorf("fact id %q was used twice", fact.ID)
		}
		ids[fact.ID] = struct{}{}
		if _, ok := facts.Lookup(fact.ID); !ok {
			t.Errorf("Lookup(%q) found nothing, so prose could not cite it", fact.ID)
		}
	}
}

// TestExtractFacts_ReadsOnlyTheTopLevelComposeKeysAsServices is the defect a
// run against a project nobody had tried it on surfaced: every key a service
// declares is indented under it, so taking any indented key reported `command`,
// `ports` and `environment` as services of their own.
func TestExtractFacts_ReadsOnlyTheTopLevelComposeKeysAsServices(t *testing.T) {
	facts := extract(t, map[string]string{
		"docker-compose.yml": `version: '3.4'
services:
  weaviate:
    command:
    - --host
    - 0.0.0.0
    image: cr.weaviate.io/semitechnologies/weaviate:1.25.2
    ports:
    - 9035:9035
    environment:
      QUERY_DEFAULTS_LIMIT: 25
  api:
    build: .
volumes:
  data:
`,
	})
	services := quoted(facts, FactService)
	if len(services) != 2 || !has(facts, FactService, "weaviate (") || !has(facts, FactService, "api (") {
		t.Fatalf("services = %v, want exactly the two real services", services)
	}
	if !has(facts, FactPort, "9035:9035") {
		t.Errorf("no published port fact for an unquoted compose mapping; ports = %v", quoted(facts, FactPort))
	}
}

// TestExtractFacts_SkipsTestsAndFixtures matters because a route registered in
// a test is a fixture. A guidebook that sent the examiner at one would have it
// report a bug against something the app never serves.
func TestExtractFacts_SkipsTestsAndFixtures(t *testing.T) {
	facts := extract(t, webApp)
	for _, route := range quoted(facts, FactRoute) {
		if strings.Contains(route, "/only-in-a-test") || strings.Contains(route, "/fixture-route") {
			t.Errorf("route %q came from a test or fixture file", route)
		}
	}
	if len(facts.NotScanned) == 0 {
		t.Error("NotScanned is empty, want what was skipped said outright")
	}
}

// TestExtractFacts_DoesNotReadAnImageTagAsAPort is the precision this rule
// needs: "node:20" and "postgres:16" are images, and a guidebook naming port 20
// would send the examiner somewhere nothing is listening.
func TestExtractFacts_DoesNotReadAnImageTagAsAPort(t *testing.T) {
	facts := extract(t, webApp)
	for _, address := range quoted(facts, FactListen) {
		if strings.Contains(address, "node:20") || strings.Contains(address, "postgres:16") {
			t.Errorf("listen address %q is an image tag, not an address", address)
		}
	}
}

// TestExtractFacts_IsDeterministic keeps the same commit rendering the same
// document, which is what makes a second run a draft somebody can diff.
func TestExtractFacts_IsDeterministic(t *testing.T) {
	repo := newRepo(t, webApp)
	_, first, err := Prepare(repo)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	_, second, err := Prepare(repo)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if fmt.Sprint(first) != fmt.Sprint(second) {
		t.Error("two extractions of one commit disagree, so a regenerated guidebook could not be diffed against its predecessor")
	}
}

// TestExtractFacts_NamesWhatItCapped is the no-silent-caps rule: a guidebook
// written over forty of four hundred routes must not read like one written
// over all of them.
func TestExtractFacts_NamesWhatItCapped(t *testing.T) {
	var routes strings.Builder
	routes.WriteString("const app = express()\n")
	for n := 0; n < maxFactsPerCategory+5; n++ {
		fmt.Fprintf(&routes, "app.get('/route-%d', handler)\n", n)
	}
	facts := extract(t, map[string]string{"server.js": routes.String()})

	if listed := len(quoted(facts, FactRoute)); listed != maxFactsPerCategory {
		t.Errorf("listed %d routes, want the cap of %d", listed, maxFactsPerCategory)
	}
	capped := strings.Join(facts.Truncated, " ")
	if !strings.Contains(capped, string(FactRoute)) || !strings.Contains(capped, "not listed") {
		t.Errorf("Truncated = %v, want it to name the capped category and the count dropped", facts.Truncated)
	}
	if !strings.Contains(facts.Render(), "Capped categories") {
		t.Error("the rendered appendix does not say a category was capped")
	}
}

func TestPrepare_RefusesAProjectWithNothingToRead(t *testing.T) {
	repo := newRepo(t, map[string]string{"logo.bin": "\x00\x01\x02"})
	_, _, err := Prepare(repo)
	if err == nil || !strings.Contains(err.Error(), "nothing to learn how to operate this project from") {
		t.Fatalf("Prepare() error = %v, want a refusal in the guidebook's own words", err)
	}
}

func TestFactsRender_SaysSoWhenItFoundNothing(t *testing.T) {
	rendered := Facts{}.Render()
	if !strings.Contains(rendered, "no facts from this project") && !strings.Contains(rendered, "extracted no facts") {
		t.Errorf("Render() = %q, want it to say outright that nothing was extracted", rendered)
	}
}

// TestExtractFacts_UsesTheDirectoryNameWithoutAManifest keeps a project with no
// manifest at all nameable.
func TestExtractFacts_UsesTheDirectoryNameWithoutAManifest(t *testing.T) {
	dir := newRepo(t, map[string]string{"README.md": "an app\n"})
	corpus, err := documenter.LoadCorpus(dir)
	if err != nil {
		t.Fatalf("LoadCorpus() error = %v", err)
	}
	facts := ExtractFacts(corpus, "")
	if facts.Project != filepath.Base(corpus.Root()) {
		t.Errorf("Project = %q, want the checkout's directory name %q", facts.Project, filepath.Base(corpus.Root()))
	}
}
