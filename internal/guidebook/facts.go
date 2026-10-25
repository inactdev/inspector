package guidebook

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/inactdev/inspector/internal/documenter"
)

// maxFactsPerCategory bounds one category's listed facts. A repository with
// four hundred routes would otherwise bury every other fact in the output and
// cost the model its context. What was dropped is counted and named rather than
// silently cut: a guidebook grounded on a quarter of the routes must not read
// like one grounded on all of them.
const maxFactsPerCategory = 40

// maxDetailWidth bounds one fact's quoted text, which for a listen address or a
// Dockerfile directive is the source line itself.
const maxDetailWidth = 160

// FactCategory names which deterministic question a fact answers. These are the
// questions a guidebook has to answer and that source can answer without
// interpretation: not "what does this app do", which is the model's job, but
// "what command does this project declare" and "what path does this code
// register".
type FactCategory string

const (
	FactCheckCommand FactCategory = "check command"
	FactManifest     FactCategory = "manifest"
	FactRunScript    FactCategory = "run script"
	FactContainer    FactCategory = "container image"
	FactService      FactCategory = "compose service"
	FactPort         FactCategory = "published port"
	FactRoute        FactCategory = "http route"
	FactEntryPoint   FactCategory = "cli entry point"
	FactFlag         FactCategory = "command flag"
	FactEnvironment  FactCategory = "environment variable"
	FactListen       FactCategory = "listen address"
)

// factCategoryOrder fixes both the order facts are listed in and the order they
// are numbered in, so the same commit always produces the same fact ids.
var factCategoryOrder = []FactCategory{
	FactCheckCommand, FactContainer, FactManifest, FactRunScript,
	FactService, FactPort, FactListen, FactRoute, FactEntryPoint, FactFlag, FactEnvironment,
}

// Fact is one thing inspector read out of the project's own files, with the
// place it was read from. A fact is never an interpretation: it is quoted text
// plus a path and line, so a person confirming the guidebook can check it
// without taking anybody's word for it.
type Fact struct {
	ID       string       `json:"id"`
	Category FactCategory `json:"category"`
	Detail   string       `json:"detail"`
	Path     string       `json:"path"`
	Line     int          `json:"line"`
}

// Facts is everything inspector extracted from one project, deterministically.
type Facts struct {
	// Project is the project's own name for itself, from its manifest where it
	// has one and its directory name otherwise.
	Project string `json:"project"`
	// Commit is the commit the facts were read at, so a guidebook can say which
	// version of the project it describes.
	Commit string `json:"commit,omitempty"`
	Facts  []Fact `json:"facts"`
	// Truncated names every category whose facts were capped, with the real
	// count. Never empty when something was dropped.
	Truncated []string `json:"truncated,omitempty"`
	// Scanned and NotScanned record how much of the project the line-by-line
	// rules looked at, and what they deliberately skipped.
	Scanned    int      `json:"scannedFiles"`
	NotScanned []string `json:"notScanned,omitempty"`
}

// Lookup finds a fact by the id the guidebook's prose cites.
func (f Facts) Lookup(id string) (Fact, bool) {
	for _, fact := range f.Facts {
		if fact.ID == id {
			return fact, true
		}
	}
	return Fact{}, false
}

// Categories counts the facts per category, for the run's own report.
func (f Facts) Categories() map[string]int {
	counts := make(map[string]int)
	for _, fact := range f.Facts {
		counts[string(fact.Category)]++
	}
	return counts
}

// ExtractFacts reads the project's tracked text and returns what it can
// establish without interpreting anything. This runs before the model sees
// anything, and its output is both the model's grounding and an appendix of the
// finished guidebook - the line between what inspector read and what the model
// concluded is the one thing a confirming human most needs to see.
func ExtractFacts(corpus *documenter.Corpus, commit string) Facts {
	facts := Facts{Commit: commit}
	collected := map[FactCategory][]Fact{}
	counts := map[FactCategory]int{}
	add := func(category FactCategory, detail, filePath string, line int) {
		detail = strings.TrimSpace(detail)
		if detail == "" {
			return
		}
		counts[category]++
		if len(collected[category]) >= maxFactsPerCategory {
			return
		}
		collected[category] = append(collected[category], Fact{
			Category: category, Detail: truncate(detail, maxDetailWidth), Path: filePath, Line: line,
		})
	}

	var skipped []string
	for _, filePath := range corpus.Paths() {
		lines, ok := corpus.Lines(filePath)
		if !ok {
			continue
		}
		if reason, skip := notScannable(filePath); skip {
			// Nothing from a skipped file, not even its manifest: a package.json
			// under testdata or a go.mod under vendor declares something about a
			// fixture or a dependency, not about the project a person operates.
			skipped = appendUnique(skipped, reason)
			continue
		}
		facts.Scanned++
		extractFileFacts(filePath, lines, add)
		for number, line := range lines {
			for _, rule := range lineRules {
				if rule.appliesTo != nil && !rule.appliesTo(filePath) {
					continue
				}
				for _, match := range rule.pattern.FindAllStringSubmatch(line, -1) {
					add(rule.category, rule.detail(match, line), filePath, number+1)
				}
			}
		}
	}

	facts.Project = projectName(corpus, collected)
	for _, category := range factCategoryOrder {
		for _, fact := range collected[category] {
			fact.ID = fmt.Sprintf("f%d", len(facts.Facts)+1)
			facts.Facts = append(facts.Facts, fact)
		}
		if dropped := counts[category] - len(collected[category]); dropped > 0 {
			facts.Truncated = append(facts.Truncated, fmt.Sprintf("%s: %d found, the first %d listed, %d not listed", category, counts[category], len(collected[category]), dropped))
		}
	}
	sort.Strings(skipped)
	facts.NotScanned = skipped
	return facts
}

// lineRule is one deterministic pattern over one line of project text.
type lineRule struct {
	category  FactCategory
	pattern   *regexp.Regexp
	detail    func(match []string, line string) string
	appliesTo func(path string) bool
}

// capture renders a fact from one submatch group.
func capture(group int) func([]string, string) string {
	return func(match []string, _ string) string {
		if group >= len(match) {
			return ""
		}
		return match[group]
	}
}

// wholeLine renders the source line itself. It is the right detail wherever the
// useful fact is the expression rather than one name in it - a listen call's
// address argument, a Dockerfile directive - and it keeps inspector from
// pretending to parse a language it does not parse.
func wholeLine(match []string, line string) string {
	_ = match
	return strings.TrimSpace(line)
}

var lineRules = []lineRule{
	{
		// Method-named route registration, which covers Express, gin, chi,
		// echo, Fiber and every client-side fetch of the app's own path - a
		// call to a route is evidence of the route.
		category: FactRoute,
		pattern:  regexp.MustCompile("(?i)\\b(get|post|put|patch|delete|head|options)\\s*\\(\\s*[\"'`](/[^\"'`\\s]*)[\"'`]"),
		detail: func(match []string, _ string) string {
			return strings.ToUpper(match[1]) + " " + match[2]
		},
	},
	{
		// net/http and any other mux that takes the path without a method.
		category: FactRoute,
		pattern:  regexp.MustCompile("(?i)\\b(?:handlefunc|handle|mount|pathprefix)\\s*\\(\\s*[\"'`](/[^\"'`\\s]*)[\"'`]"),
		detail: func(match []string, _ string) string {
			return "(any method) " + match[1]
		},
	},
	{
		// Decorator routing: Flask, FastAPI, Starlette, Sanic.
		category: FactRoute,
		pattern:  regexp.MustCompile(`(?i)@\w+\.(get|post|put|patch|delete|route)\s*\(\s*["'](/[^"'\s]*)["']`),
		detail: func(match []string, _ string) string {
			if strings.EqualFold(match[1], "route") {
				return "(any method) " + match[2]
			}
			return strings.ToUpper(match[1]) + " " + match[2]
		},
	},
	{
		// Spring and Micronaut annotations.
		category: FactRoute,
		pattern:  regexp.MustCompile(`@(Get|Post|Put|Patch|Delete|Request)Mapping\s*\(\s*(?:value\s*=\s*)?"(/[^"\s]*)"`),
		detail: func(match []string, _ string) string {
			if match[1] == "Request" {
				return "(any method) " + match[2]
			}
			return strings.ToUpper(match[1]) + " " + match[2]
		},
	},
	{
		category: FactListen,
		pattern:  regexp.MustCompile(`(?i)(?:ListenAndServe|ListenAndServeTLS|net\.Listen|http\.Serve|createServer|\.listen\(|app\.run\(|uvicorn\.run\(|\.bind\(|Kestrel|runserver)`),
		detail:   wholeLine,
	},
	{
		// A quoted host:port or :port. The host half has to look like a host -
		// empty, localhost, an IP, or something dotted - because "node:20" and
		// "postgres:16" are image tags, and a guidebook that sent the examiner
		// at port 20 would be worse than one that named no port at all.
		category: FactListen,
		pattern:  regexp.MustCompile(`["'` + "`" + `]((?:localhost|[0-9]{1,3}(?:\.[0-9]{1,3}){3}|[A-Za-z0-9\-]+(?:\.[A-Za-z0-9\-]+)+)?:\d{2,5})["'` + "`" + `]`),
		detail:   capture(1),
		appliesTo: func(filePath string) bool {
			return !isComposeFile(filePath) && !isDockerfile(filePath)
		},
	},
	{
		// The same literal inside a compose file or Dockerfile is a published
		// port rather than something the app listens on itself.
		category: FactPort,
		pattern:  regexp.MustCompile(`(?i)(?:^\s*-\s*["']?(\d{2,5}:\d{2,5})["']?\s*$|^\s*EXPOSE\s+(\d{2,5}))`),
		detail: func(match []string, _ string) string {
			if match[1] != "" {
				return match[1]
			}
			return "EXPOSE " + match[2]
		},
		appliesTo: func(filePath string) bool {
			return isComposeFile(filePath) || isDockerfile(filePath)
		},
	},
	{
		category:  FactContainer,
		pattern:   regexp.MustCompile(`^\s*(FROM|EXPOSE|ENTRYPOINT|CMD|USER|WORKDIR|HEALTHCHECK)\s+\S`),
		detail:    wholeLine,
		appliesTo: isDockerfile,
	},
	{
		category: FactEntryPoint,
		pattern:  regexp.MustCompile(`(?:flag\.NewFlagSet|NewFlagSet)\s*\(\s*"([^"]+)"`),
		detail: func(match []string, _ string) string {
			return "command line: " + match[1]
		},
	},
	{
		category: FactEntryPoint,
		pattern:  regexp.MustCompile(`(?i)(?:Use|command|program)\s*[:=]\s*["']([a-z][a-z0-9 _.<>\[\]-]*)["']`),
		detail: func(match []string, _ string) string {
			return "command line: " + match[1]
		},
	},
	{
		category: FactFlag,
		pattern:  regexp.MustCompile(`(?:add_argument|\.option|\.flag|\.addOption)\s*\(\s*["']((?:--|-)[A-Za-z0-9][A-Za-z0-9_.-]*)["']`),
		detail:   capture(1),
	},
	{
		// Go's flag package names a flag without its dashes.
		category: FactFlag,
		pattern:  regexp.MustCompile(`(?:fs|flag|flags|f)\.(?:String|Bool|Int|Int64|Uint|Float64|Duration|Var)(?:Var)?\s*\(\s*(?:&\w+\s*,\s*)?"([A-Za-z0-9][A-Za-z0-9_.-]*)"`),
		detail: func(match []string, _ string) string {
			return "--" + match[1]
		},
	},
	{
		category: FactEnvironment,
		pattern:  regexp.MustCompile(`(?:os\.Getenv|System\.getenv|getenv)\s*\(\s*["']([A-Za-z_][A-Za-z0-9_]*)["']`),
		detail:   capture(1),
	},
	{
		category: FactEnvironment,
		pattern:  regexp.MustCompile(`process\.env(?:\.([A-Za-z_][A-Za-z0-9_]*)|\[\s*["']([A-Za-z_][A-Za-z0-9_]*)["']\s*\])`),
		detail: func(match []string, _ string) string {
			if match[1] != "" {
				return match[1]
			}
			return match[2]
		},
	},
	{
		category: FactEnvironment,
		pattern:  regexp.MustCompile(`(?:os\.environ|ENV)(?:\.get)?\s*[\(\[]\s*["']([A-Za-z_][A-Za-z0-9_]*)["']`),
		detail:   capture(1),
	},
	{
		// A variable name held in a named constant, which is how a careful
		// project reads its environment: os.Getenv(GitHubTokenEnvVar) tells a
		// literal-matching rule nothing, and GITHUB_TOKEN is exactly the kind of
		// thing a guidebook must not omit. Requires both the identifier to say
		// env and the value to be a shouting name, so it stays narrow.
		category: FactEnvironment,
		pattern:  regexp.MustCompile(`(?i)\w*env(?:var|variable|name)?\w*\s*=\s*"([A-Z][A-Z0-9_]{2,})"`),
		detail:   capture(1),
	},
}

// extractFileFacts handles the declarations that are structure rather than
// lines: a manifest's fields, a compose file's service block, the project's own
// inspector configuration. It runs only on files the scan did not skip.
func extractFileFacts(filePath string, lines []string, add func(FactCategory, string, string, int)) {
	content := strings.Join(lines, "\n")
	base := path.Base(filePath)
	switch {
	case filePath == ".inspector.json":
		extractInspectorConfig(filePath, content, lines, add)
	case base == "package.json" && !strings.Contains(filePath, "node_modules/"):
		extractPackageJSON(filePath, content, lines, add)
	case base == "go.mod":
		for number, line := range lines {
			if module := strings.TrimPrefix(line, "module "); module != line {
				add(FactManifest, "Go module "+strings.TrimSpace(module), filePath, number+1)
			}
		}
	case base == "Makefile" || base == "makefile" || base == "GNUmakefile":
		add(FactManifest, "a Makefile declares this project's targets", filePath, 1)
		for number, line := range lines {
			if match := makeTarget.FindStringSubmatch(line); match != nil {
				add(FactRunScript, "make "+match[1], filePath, number+1)
			}
		}
	case isComposeFile(filePath):
		extractComposeServices(filePath, lines, add)
	case isEnvExample(filePath):
		for number, line := range lines {
			if match := envAssignment.FindStringSubmatch(line); match != nil {
				add(FactEnvironment, match[1]+" (named in "+base+")", filePath, number+1)
			}
		}
	case otherManifests[base]:
		add(FactManifest, base+" declares this project's dependencies or build", filePath, 1)
	}
	if isRunScriptPath(filePath) {
		detail := filePath
		if len(lines) > 0 && strings.HasPrefix(lines[0], "#!") {
			detail = filePath + " (" + strings.TrimSpace(lines[0]) + ")"
		}
		add(FactRunScript, detail, filePath, 1)
	}
	// A Go command is its directory, which no line in the file states.
	if match := goCommandMain.FindStringSubmatch(filePath); match != nil {
		add(FactEntryPoint, "command line: go run ./cmd/"+match[1], filePath, 1)
	}
}

func extractInspectorConfig(filePath, content string, lines []string, add func(FactCategory, string, string, int)) {
	var config struct {
		Check   string `json:"check"`
		Image   string `json:"image"`
		Network bool   `json:"network"`
	}
	if err := json.Unmarshal([]byte(content), &config); err != nil {
		return
	}
	if config.Check != "" {
		add(FactCheckCommand, "the project's own check command is: "+config.Check, filePath, lineOf(lines, `"check"`))
	}
	if config.Image != "" {
		add(FactContainer, "checks run in the image "+config.Image, filePath, lineOf(lines, `"image"`))
	}
	if config.Network {
		add(FactCheckCommand, "the check container is given network access by this project's configuration", filePath, lineOf(lines, `"network"`))
	}
}

func extractPackageJSON(filePath, content string, lines []string, add func(FactCategory, string, string, int)) {
	var manifest struct {
		Name    string            `json:"name"`
		Scripts map[string]string `json:"scripts"`
		Bin     json.RawMessage   `json:"bin"`
	}
	if err := json.Unmarshal([]byte(content), &manifest); err != nil {
		return
	}
	if manifest.Name != "" {
		add(FactManifest, "npm package "+manifest.Name, filePath, lineOf(lines, `"name"`))
	}
	for _, name := range sortedKeys(manifest.Scripts) {
		add(FactRunScript, fmt.Sprintf("npm run %s -> %s", name, manifest.Scripts[name]), filePath, lineOf(lines, `"`+name+`"`))
	}
	var binByName map[string]string
	if err := json.Unmarshal(manifest.Bin, &binByName); err == nil {
		for _, name := range sortedKeys(binByName) {
			add(FactEntryPoint, "command line: "+name, filePath, lineOf(lines, `"`+name+`"`))
		}
		return
	}
	var binPath string
	if err := json.Unmarshal(manifest.Bin, &binPath); err == nil && binPath != "" && manifest.Name != "" {
		add(FactEntryPoint, "command line: "+manifest.Name, filePath, lineOf(lines, `"bin"`))
	}
}

// extractComposeServices reads the service names and nothing else. Compose
// files are YAML and inspector does not parse YAML, so it reads what the
// indentation says: a service is a key at the FIRST level under `services:`,
// and every key below that level belongs to a service rather than being one.
// Taking any indented key would report `ports` and `environment` as services -
// which is what it did until a run against a project nobody had tried it on
// said so.
func extractComposeServices(filePath string, lines []string, add func(FactCategory, string, string, int)) {
	inServices, serviceIndent := false, -1
	for number, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "services:" {
			inServices, serviceIndent = true, -1
			continue
		}
		if !inServices || trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if indent == 0 {
			// Back at the top level, so this is a different section.
			inServices = false
			continue
		}
		match := composeService.FindStringSubmatch(trimmed)
		if match == nil {
			continue
		}
		if serviceIndent < 0 {
			serviceIndent = indent
		}
		if indent != serviceIndent {
			continue
		}
		add(FactService, match[1]+" (compose service)", filePath, number+1)
	}
}

var (
	makeTarget     = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9_.\-]*):(?:\s|$)`)
	composeService = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9_.\-]*):$`)
	envAssignment  = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=`)
	goCommandMain  = regexp.MustCompile(`^cmd/([^/]+)/main\.go$`)
)

var otherManifests = map[string]bool{
	"Cargo.toml": true, "pyproject.toml": true, "requirements.txt": true, "Pipfile": true,
	"Gemfile": true, "pom.xml": true, "build.gradle": true, "build.gradle.kts": true,
	"composer.json": true, "Package.swift": true, "pubspec.yaml": true, "mix.exs": true,
	"setup.py": true, "CMakeLists.txt": true,
}

// notScannable names the files the line-by-line rules deliberately skip, and
// why. Tests and fixtures are the important ones: a route registered in a test
// is a fixture, and a guidebook that sent the examiner at it would be teaching
// it to drive something the app does not serve.
func notScannable(filePath string) (string, bool) {
	lower := strings.ToLower(filePath)
	for _, directory := range []string{"vendor/", "node_modules/", "testdata/", "fixtures/", "third_party/", ".git/"} {
		if strings.HasPrefix(lower, directory) || strings.Contains(lower, "/"+directory) {
			return "vendored, generated and fixture directories (" + strings.TrimSuffix(directory, "/") + ")", true
		}
	}
	base := path.Base(lower)
	switch {
	case strings.Contains(base, "_test.") || strings.HasPrefix(base, "test_") || strings.Contains(base, ".test.") || strings.Contains(base, ".spec."):
		return "test files, whose routes and commands are fixtures rather than things the app serves", true
	case strings.HasSuffix(base, ".min.js") || strings.HasSuffix(base, ".lock") || base == "package-lock.json" || base == "go.sum":
		return "lock files and minified bundles, which declare nothing a person operates", true
	}
	return "", false
}

func isComposeFile(filePath string) bool {
	base := strings.ToLower(path.Base(filePath))
	return strings.HasPrefix(base, "docker-compose") || strings.HasPrefix(base, "compose.")
}

func isDockerfile(filePath string) bool {
	base := strings.ToLower(path.Base(filePath))
	return base == "dockerfile" || strings.HasPrefix(base, "dockerfile.") || strings.HasSuffix(base, ".dockerfile")
}

func isEnvExample(filePath string) bool {
	base := strings.ToLower(path.Base(filePath))
	return strings.HasPrefix(base, ".env.") || base == "env.example" || base == ".env.example"
}

// isRunScriptPath recognizes where projects keep the commands a person runs.
// By location and extension rather than by file mode: the corpus is the
// project's tracked text, and a committed script's executable bit is not what
// tells a reader it is the way in.
func isRunScriptPath(filePath string) bool {
	if _, skip := notScannable(filePath); skip {
		return false
	}
	for _, directory := range []string{"script/", "scripts/", "bin/"} {
		if strings.HasPrefix(filePath, directory) {
			return true
		}
	}
	return !strings.Contains(filePath, "/") && strings.HasSuffix(filePath, ".sh")
}

// projectName is the project's own name for itself. A manifest name beats the
// directory name, because a checkout directory is whatever somebody called it.
func projectName(corpus *documenter.Corpus, collected map[FactCategory][]Fact) string {
	for _, fact := range collected[FactManifest] {
		if name := strings.TrimPrefix(fact.Detail, "npm package "); name != fact.Detail {
			return name
		}
	}
	for _, fact := range collected[FactManifest] {
		if module := strings.TrimPrefix(fact.Detail, "Go module "); module != fact.Detail {
			return path.Base(module)
		}
	}
	return path.Base(strings.ReplaceAll(corpus.Root(), "\\", "/"))
}

// Render is the facts appendix of the finished guidebook: every fact, verbatim,
// with the path and line it came from. This is what makes the document
// checkable - the prose above it is a model's reading of these lines, and a
// reader who disagrees can go to the line and see for themselves.
func (f Facts) Render() string {
	var b strings.Builder
	b.WriteString("These are the facts Inspector read out of the project's own files, with the\nplace each one came from. Everything above this section is a model's reading of\nthem; everything in it is quoted text. Check these first.\n\n")
	if f.Commit != "" {
		fmt.Fprintf(&b, "Read at commit `%s`, across %d scanned file(s).\n\n", f.Commit, f.Scanned)
	}
	current := FactCategory("")
	for _, fact := range f.Facts {
		if fact.Category != current {
			if current != "" {
				b.WriteString("\n")
			}
			current = fact.Category
			fmt.Fprintf(&b, "**%s**\n\n", current)
		}
		fmt.Fprintf(&b, "- `%s` `%s` - `%s:%d`\n", fact.ID, fact.Detail, fact.Path, fact.Line)
	}
	if len(f.Facts) == 0 {
		b.WriteString("Inspector extracted no facts from this project at all. Treat every command\nabove as a guess until you have run it yourself.\n")
	}
	if len(f.Truncated) > 0 {
		b.WriteString("\nCapped categories - this guidebook was written over a listed subset, not all of them:\n\n")
		for _, truncated := range f.Truncated {
			fmt.Fprintf(&b, "- %s\n", truncated)
		}
	}
	if len(f.NotScanned) > 0 {
		b.WriteString("\nNot scanned for routes, commands or flags:\n\n")
		for _, reason := range f.NotScanned {
			fmt.Fprintf(&b, "- %s\n", reason)
		}
	}
	return b.String()
}

// Describe is the facts as the model is told about them, with the duty that
// comes with them: cite the id, never invent one.
func (f Facts) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "EXTRACTED FACTS (Inspector read these out of the project's tracked files itself; cite them by id in groundedIn, and never invent an id):\n\n")
	current := FactCategory("")
	for _, fact := range f.Facts {
		if fact.Category != current {
			current = fact.Category
			fmt.Fprintf(&b, "  %s:\n", current)
		}
		fmt.Fprintf(&b, "    %s  %s  (%s:%d)\n", fact.ID, fact.Detail, fact.Path, fact.Line)
	}
	if len(f.Facts) == 0 {
		b.WriteString("  none - inspector could not extract a single fact from this project.\n")
	}
	for _, truncated := range f.Truncated {
		fmt.Fprintf(&b, "  capped: %s\n", truncated)
	}
	b.WriteString("\n")
	return b.String()
}

func lineOf(lines []string, needle string) int {
	for number, line := range lines {
		if strings.Contains(line, needle) {
			return number + 1
		}
	}
	return 1
}

func sortedKeys(entries map[string]string) []string {
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "..."
}
