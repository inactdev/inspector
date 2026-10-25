// Package guidebook generates the examiner's operating instructions for a
// project (issue #22).
//
// The examiner (issue #7) is denied the repository on purpose: it judges a
// running app against the request, with no diff and no source. That restriction
// is the whole mechanism, and it leaves a gap - an examiner that cannot read the
// code also cannot discover from the code how to start the app, reach a screen,
// or call an endpoint. Something that IS allowed to read the repository has to
// convert code knowledge into operating instructions once, at setup time. That
// is this package.
//
// It is the generator, not any one project's guidebook. Two things keep its
// output honest:
//
//   - Inspector extracts the deterministic facts itself - the check command,
//     manifests, run scripts, compose services, published ports, routes, CLI
//     entry points, environment variables - each with the path and line it came
//     from, and lists them verbatim in the finished document. The prose must
//     cite them by id. A reader can therefore tell what was read from what was
//     concluded, which is the only way a confirming human can confirm anything.
//   - Nothing it writes is authority. A generated guidebook says
//     `confirmed: no` until a person replaces that line with a date and a name,
//     and the examiner records every capability driven from an unconfirmed
//     guidebook as could-not-be-tested rather than as a pass.
//
// The ordering trap issue #22 names is structural here: generating a guidebook
// reads the repository, examining with one does not, and the two never happen
// in the same command. Inspector has no code path that regenerates a guidebook
// during an examination.
package guidebook

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/inactdev/inspector/internal/documenter"
)

// FileName is where a confirmed guidebook lives: the target repository's root,
// beside .inspector.json, committed. The examiner is given this file; the
// repository it describes is exactly what the examiner never sees.
const FileName = "INSPECTOR-GUIDEBOOK.md"

// DraftFileName is where a regenerated guidebook lands when FileName already
// exists. The real file is never written over - a human may have edited and
// confirmed it, and that work is not inspector's to discard.
const DraftFileName = "INSPECTOR-GUIDEBOOK.draft.md"

// SectionName names one of the guidebook's fixed top-level sections.
type SectionName string

const (
	// Launch is the real commands from a clean checkout, including whatever
	// runtime isolation the project requires.
	Launch SectionName = "Launch"
	// Doctor is how to tell the app is actually up and serving before anything
	// is judged against it. Without it, a dead app reads as a broken feature.
	Doctor SectionName = "Doctor"
	// Drive is how the harness talks to the app at all: base URL, auth,
	// content types, the shape of a call.
	Drive SectionName = "Drive"
	// Evidence is where state lands and what success and failure look like, so
	// a verdict rests on something checkable.
	Evidence SectionName = "Evidence"
	// Cleanup is how to put the machine back, so running an examination twice
	// is as safe as running it once.
	Cleanup SectionName = "Cleanup"
	// CannotBeTested is the environment's limits: what must be reported as
	// could-not-be-tested rather than failed. It is a section of its own
	// because it is the examiner's only defence against calling a limit of the
	// harness a defect of the app.
	CannotBeTested SectionName = "Cannot be tested here"
)

// RequiredSections is every top-level section, in the order they are rendered.
// A guidebook missing one of these is rejected rather than written: each
// answers a question the examiner must have answered before it can judge
// anything, and a missing section is where it would guess instead.
var RequiredSections = []SectionName{Launch, Doctor, Drive, Evidence, Cleanup, CannotBeTested}

// commandSections must contain a runnable command. These two are the ones the
// examiner's operator literally executes before any judgment happens, so prose
// alone is not enough - "start it the usual way" has never started anything.
var commandSections = map[SectionName]bool{Launch: true, Doctor: true}

// Section is one top-level section's body, plus the facts it rests on.
type Section struct {
	Name SectionName `json:"name"`
	Body string      `json:"body"`
	// GroundedIn cites the extracted facts this section was written from.
	GroundedIn []string `json:"groundedIn,omitempty"`
}

// Feature is one user-facing feature, with exactly the four headings the
// examiner needs and nothing else. The shape is fixed rather than free prose
// because the examiner reads it per feature: what else is in here, how a person
// gets to it, how the harness drives it, and what will mislead you.
type Feature struct {
	Name string `json:"name"`
	// SubFeatures is what else lives inside this feature.
	SubFeatures string `json:"subFeatures"`
	// HowToReachIt is the route to the feature in a user's terms, not a module
	// path: what somebody opens, types, or calls to arrive at it.
	HowToReachIt string `json:"howToReachIt"`
	// DrivingIt is the concrete endpoint call, CLI invocation, or UI step the
	// harness performs.
	DrivingIt string `json:"drivingIt"`
	// Gotchas is what will mislead a judge who has never seen the code:
	// required ordering, state left behind, a success that looks like a
	// failure.
	Gotchas string `json:"gotchas"`
	// GroundedIn cites the extracted facts this feature was derived from.
	GroundedIn []string `json:"groundedIn"`
}

// Guidebook is the validated document, before rendering. Inspector renders the
// markdown from this; the model never produces markdown structure.
type Guidebook struct {
	Facts    Facts                    `json:"facts"`
	Sections map[SectionName]*Section `json:"sections"`
	Features []Feature                `json:"features"`
	// Vendor and Model name who wrote the prose, recorded in the document
	// itself so the choice is never silent - the same rule the examiner, the
	// reviewer and the documenter follow.
	Vendor string `json:"vendor"`
	Model  string `json:"model"`
}

// ResultKind is the overall outcome of one generation run.
type ResultKind string

const (
	// Generated means a guidebook was written. It is never a confirmed one:
	// confirmation is a person's act, performed after this command returns.
	Generated ResultKind = "generated"
	// Refused means no guidebook was written at all.
	Refused ResultKind = "refused"
)

// Result is what one generation run reports. Nothing here gates a merge:
// generating a guidebook is a setup-time act, like the installer (issue #5).
type Result struct {
	Kind    ResultKind `json:"kind"`
	Project string     `json:"project,omitempty"`
	Commit  string     `json:"commit,omitempty"`
	Vendor  string     `json:"vendor,omitempty"`
	Model   string     `json:"model,omitempty"`
	// Path is the repository-relative file that was written.
	Path string `json:"path,omitempty"`
	// Draft is true when FileName already existed, so this run wrote
	// DraftFileName instead of touching it.
	Draft bool `json:"draft"`
	// ReplacedDraft is true when an earlier draft was overwritten by this one.
	ReplacedDraft bool `json:"replacedDraft,omitempty"`
	// Features names every feature the guidebook covers, so a caller can see
	// the map without opening the file.
	Features []string `json:"features,omitempty"`
	// Facts counts what grounded it, by category.
	Facts    int                      `json:"facts"`
	FactsBy  map[string]int           `json:"factsByCategory,omitempty"`
	FactsCap []string                 `json:"factsCapped,omitempty"`
	Corpus   documenter.CorpusSummary `json:"corpus"`
	Lookups  int                      `json:"corpusLookups"`
	Message  string                   `json:"message,omitempty"`
}

// Record owns the proposed guidebook for one run and validates every piece
// against the extracted facts before accepting it. Same division as every other
// Inspector judgment: the model proposes, inspector decides what is recordable.
type Record struct {
	corpus   *documenter.Corpus
	facts    Facts
	sections map[SectionName]*Section
	features []Feature
	names    map[string]struct{}
}

// NewRecord starts a record that accepts only sections and features grounded in
// facts inspector itself extracted.
func NewRecord(corpus *documenter.Corpus, facts Facts) *Record {
	return &Record{
		corpus:   corpus,
		facts:    facts,
		sections: make(map[SectionName]*Section, len(RequiredSections)),
		names:    make(map[string]struct{}),
	}
}

// AddSections validates and stores a batch of top-level sections atomically.
func (r *Record) AddSections(sections []Section) error {
	if len(sections) == 0 {
		return errors.New("no sections were given")
	}
	validated := make([]Section, 0, len(sections))
	for _, section := range sections {
		if err := r.validateSection(&section); err != nil {
			return err
		}
		validated = append(validated, section)
	}
	for _, section := range validated {
		stored := section
		r.sections[section.Name] = &stored
	}
	return nil
}

func (r *Record) validateSection(section *Section) error {
	if !isRequiredSection(section.Name) {
		return fmt.Errorf("section %q is not one of the guidebook's sections (%s); the shape is fixed and inspector renders it", section.Name, joinSections())
	}
	if strings.TrimSpace(section.Body) == "" {
		return fmt.Errorf("section %q is empty", section.Name)
	}
	if commandSections[section.Name] && !hasCodeBlock(section.Body) {
		// The examiner's operator runs these. A section that names no command
		// is the gap this whole issue exists to close.
		return fmt.Errorf("section %q contains no fenced command block; it has to carry the real command somebody runs, not a description of it", section.Name)
	}
	grounded, err := r.validateGrounding(section.GroundedIn, fmt.Sprintf("section %q", section.Name), section.Name == Launch)
	if err != nil {
		return err
	}
	section.GroundedIn = grounded
	return nil
}

// AddFeatures validates and stores a batch of features atomically, so a partly
// wrong batch is never split into an accepted prefix and a rejected remainder.
func (r *Record) AddFeatures(features []Feature) error {
	if len(features) == 0 {
		return errors.New("no features were given")
	}
	validated := make([]Feature, 0, len(features))
	seen := make(map[string]struct{}, len(features))
	for _, feature := range features {
		if err := r.validateFeature(&feature); err != nil {
			return err
		}
		key := strings.ToLower(strings.TrimSpace(feature.Name))
		if _, exists := seen[key]; exists {
			return fmt.Errorf("feature %q appears twice in the same batch", feature.Name)
		}
		seen[key] = struct{}{}
		validated = append(validated, feature)
	}
	for _, feature := range validated {
		r.names[strings.ToLower(strings.TrimSpace(feature.Name))] = struct{}{}
		r.features = append(r.features, feature)
	}
	return nil
}

func (r *Record) validateFeature(feature *Feature) error {
	name := strings.TrimSpace(feature.Name)
	if name == "" {
		return errors.New("a feature has no name")
	}
	if _, exists := r.names[strings.ToLower(name)]; exists {
		return fmt.Errorf("feature %q was already recorded", name)
	}
	feature.Name = name
	for _, heading := range []struct {
		label string
		value *string
	}{
		{"sub-features", &feature.SubFeatures},
		{"how to get to it from the user's point of view", &feature.HowToReachIt},
		{"driving it with the harness", &feature.DrivingIt},
		{"gotchas", &feature.Gotchas},
	} {
		if strings.TrimSpace(*heading.value) == "" {
			// All four, always. An examiner reads them per feature, and the one
			// left blank is the one it would have guessed at.
			return fmt.Errorf("feature %q has nothing under %q; every feature needs all four headings", name, heading.label)
		}
	}
	grounded, err := r.validateGrounding(feature.GroundedIn, fmt.Sprintf("feature %q", name), true)
	if err != nil {
		return err
	}
	feature.GroundedIn = grounded
	return nil
}

// validateGrounding rejects a cited fact id that does not exist, and - where
// grounding is required - a claim resting on nothing inspector read. This is
// what keeps a confidently written invention out of the document: the model
// cannot cite a fact it made up, because the ids were assigned by inspector.
func (r *Record) validateGrounding(ids []string, what string, required bool) ([]string, error) {
	cleaned := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := r.facts.Lookup(id); !ok {
			return nil, fmt.Errorf("%s cites fact %q, which inspector did not extract; cite only the ids listed in EXTRACTED FACTS", what, id)
		}
		cleaned = append(cleaned, id)
	}
	if required && len(cleaned) == 0 {
		return nil, fmt.Errorf("%s cites no extracted fact; it has to rest on something inspector read out of the project, so a reader can check it", what)
	}
	return cleaned, nil
}

// Build assembles the validated guidebook, refusing anything an examiner could
// not work from: a missing section, or no features at all.
func (r *Record) Build(vendor, model string) (Guidebook, error) {
	var missing []string
	for _, name := range RequiredSections {
		if r.sections[name] == nil {
			missing = append(missing, string(name))
		}
	}
	if len(missing) > 0 {
		return Guidebook{}, fmt.Errorf("the guidebook has no %s section; every one of %s is required, because each answers a question the examiner would otherwise guess at", strings.Join(missing, " or "), joinSections())
	}
	if len(r.features) == 0 {
		return Guidebook{}, errors.New("the guidebook names no features; a feature map with no features teaches the examiner nothing, which is the state issue #22 exists to end")
	}
	features := make([]Feature, len(r.features))
	copy(features, r.features)
	sort.SliceStable(features, func(i, j int) bool { return features[i].Name < features[j].Name })
	return Guidebook{
		Facts:    r.facts,
		Sections: r.sections,
		Features: features,
		Vendor:   vendor,
		Model:    model,
	}, nil
}

// FeatureNames is the feature map in one line, for the run's own report.
func (g Guidebook) FeatureNames() []string {
	names := make([]string, 0, len(g.Features))
	for _, feature := range g.Features {
		names = append(names, feature.Name)
	}
	return names
}

// codeBlock matches a fenced block whose first content character is neither
// space nor the closing fence's own backtick - an empty fence runs nothing, and
// "see the command below" with nothing below it is the exact failure this check
// exists to catch.
var codeBlock = regexp.MustCompile("(?s)```[^\\n]*\\n\\s*[^\\s`]")

func hasCodeBlock(body string) bool { return codeBlock.MatchString(body) }

func isRequiredSection(name SectionName) bool {
	for _, required := range RequiredSections {
		if required == name {
			return true
		}
	}
	return false
}

func joinSections() string {
	names := make([]string, 0, len(RequiredSections))
	for _, name := range RequiredSections {
		names = append(names, string(name))
	}
	return strings.Join(names, ", ")
}
