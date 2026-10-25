package guidebook

import (
	"strings"
	"testing"

	"github.com/inactdev/inspector/internal/documenter"
)

// newRecordOver builds a record over a small real project, so every grounding
// id a test cites is one inspector actually extracted.
func newRecordOver(t *testing.T) (*Record, Facts) {
	t.Helper()
	corpus, facts, err := Prepare(newRepo(t, webApp))
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	return NewRecord(corpus, facts), facts
}

func anyFactID(t *testing.T, facts Facts) string {
	t.Helper()
	if len(facts.Facts) == 0 {
		t.Fatal("the fixture project produced no facts")
	}
	return facts.Facts[0].ID
}

// completeSections is every required section, each acceptable.
func completeSections(fact string) []Section {
	sections := make([]Section, 0, len(RequiredSections))
	for _, name := range RequiredSections {
		body := "what to do for " + string(name) + "."
		if commandSections[name] {
			body += "\n\n```\nscript/check\n```\n"
		}
		sections = append(sections, Section{Name: name, Body: body, GroundedIn: []string{fact}})
	}
	return sections
}

func completeFeature(name, fact string) Feature {
	return Feature{
		Name:         name,
		SubFeatures:  "listing and creating",
		HowToReachIt: "open the app and pick Baskets",
		DrivingIt:    "POST /baskets with a JSON body",
		Gotchas:      "a repeated id updates rather than inserts",
		GroundedIn:   []string{fact},
	}
}

func TestRecord_BuildsACompleteGuidebook(t *testing.T) {
	record, facts := newRecordOver(t)
	fact := anyFactID(t, facts)
	if err := record.AddSections(completeSections(fact)); err != nil {
		t.Fatalf("AddSections() error = %v", err)
	}
	if err := record.AddFeatures([]Feature{completeFeature("Baskets", fact), completeFeature("Accounts", fact)}); err != nil {
		t.Fatalf("AddFeatures() error = %v", err)
	}
	book, err := record.Build("anthropic", "test-model")
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if len(book.Features) != 2 || book.Features[0].Name != "Accounts" {
		// Sorted, so the same content always renders the same document.
		t.Errorf("features = %v, want both, in name order", book.FeatureNames())
	}
	if book.Vendor != "anthropic" || book.Model != "test-model" {
		t.Errorf("vendor/model = %q/%q, want the caller's recorded choice", book.Vendor, book.Model)
	}
}

// TestRecord_RefusesAMissingSection is the floor: each section answers a
// question the examiner would otherwise guess at.
func TestRecord_RefusesAMissingSection(t *testing.T) {
	record, facts := newRecordOver(t)
	fact := anyFactID(t, facts)
	sections := completeSections(fact)
	if err := record.AddSections(sections[:len(sections)-1]); err != nil {
		t.Fatalf("AddSections() error = %v", err)
	}
	if err := record.AddFeatures([]Feature{completeFeature("Baskets", fact)}); err != nil {
		t.Fatalf("AddFeatures() error = %v", err)
	}
	_, err := record.Build("anthropic", "test-model")
	if err == nil || !strings.Contains(err.Error(), string(CannotBeTested)) {
		t.Fatalf("Build() error = %v, want it to name the missing section", err)
	}
}

func TestRecord_RefusesAGuidebookWithNoFeatures(t *testing.T) {
	record, facts := newRecordOver(t)
	if err := record.AddSections(completeSections(anyFactID(t, facts))); err != nil {
		t.Fatalf("AddSections() error = %v", err)
	}
	_, err := record.Build("anthropic", "test-model")
	if err == nil || !strings.Contains(err.Error(), "no features") {
		t.Fatalf("Build() error = %v, want a refusal naming the empty feature map", err)
	}
}

// TestRecord_RefusesASectionWithNoCommand is why Launch and Doctor are
// different from the rest: somebody executes them.
func TestRecord_RefusesASectionWithNoCommand(t *testing.T) {
	for _, name := range []SectionName{Launch, Doctor} {
		record, facts := newRecordOver(t)
		err := record.AddSections([]Section{{Name: name, Body: "start it the usual way", GroundedIn: []string{anyFactID(t, facts)}}})
		if err == nil || !strings.Contains(err.Error(), "fenced command block") {
			t.Fatalf("AddSections(%s) error = %v, want a refusal demanding a real command", name, err)
		}
	}
}

func TestRecord_AcceptsASectionWhoseCommandIsFenced(t *testing.T) {
	record, facts := newRecordOver(t)
	body := "run it:\n\n```sh\ndocker compose up -d\n```\n"
	if err := record.AddSections([]Section{{Name: Launch, Body: body, GroundedIn: []string{anyFactID(t, facts)}}}); err != nil {
		t.Fatalf("AddSections() error = %v", err)
	}
}

func TestRecord_RefusesAnEmptyFenceWithNoCommandInIt(t *testing.T) {
	record, facts := newRecordOver(t)
	err := record.AddSections([]Section{{Name: Doctor, Body: "check it:\n\n```\n\n```\n", GroundedIn: []string{anyFactID(t, facts)}}})
	if err == nil {
		t.Fatal("AddSections() accepted an empty code fence, which runs nothing")
	}
}

func TestRecord_RefusesASectionItDoesNotRender(t *testing.T) {
	record, _ := newRecordOver(t)
	err := record.AddSections([]Section{{Name: "Appendix", Body: "extra"}})
	if err == nil || !strings.Contains(err.Error(), "the shape is fixed") {
		t.Fatalf("AddSections() error = %v, want the document's shape held by inspector", err)
	}
}

// TestRecord_RefusesAFeatureMissingAnyOfItsFourHeadings is the per-feature
// contract: the one left blank is the one the examiner would guess at.
func TestRecord_RefusesAFeatureMissingAnyOfItsFourHeadings(t *testing.T) {
	for _, blank := range []struct {
		label string
		blank func(*Feature)
	}{
		{"sub-features", func(f *Feature) { f.SubFeatures = "" }},
		{"how to get to it", func(f *Feature) { f.HowToReachIt = "" }},
		{"driving it", func(f *Feature) { f.DrivingIt = " \n " }},
		{"gotchas", func(f *Feature) { f.Gotchas = "" }},
	} {
		record, facts := newRecordOver(t)
		feature := completeFeature("Baskets", anyFactID(t, facts))
		blank.blank(&feature)
		err := record.AddFeatures([]Feature{feature})
		if err == nil || !strings.Contains(err.Error(), "all four headings") {
			t.Errorf("AddFeatures() with no %s: error = %v, want all four required", blank.label, err)
		}
	}
}

// TestRecord_RefusesAFactItDidNotExtract is the structural guard against a
// confidently written invention: the ids were assigned by inspector, so the
// model cannot cite one for a fact it made up.
func TestRecord_RefusesAFactItDidNotExtract(t *testing.T) {
	record, facts := newRecordOver(t)
	feature := completeFeature("Baskets", "f9999")
	if err := record.AddFeatures([]Feature{feature}); err == nil || !strings.Contains(err.Error(), "which inspector did not extract") {
		t.Fatalf("AddFeatures() error = %v, want an invented fact id refused", err)
	}
	sections := completeSections(anyFactID(t, facts))
	sections[0].GroundedIn = []string{"made-up"}
	if err := record.AddSections(sections); err == nil {
		t.Fatal("AddSections() accepted an invented fact id")
	}
}

func TestRecord_RefusesAFeatureAndALaunchRestingOnNothing(t *testing.T) {
	record, facts := newRecordOver(t)
	feature := completeFeature("Baskets", "")
	feature.GroundedIn = nil
	if err := record.AddFeatures([]Feature{feature}); err == nil || !strings.Contains(err.Error(), "cites no extracted fact") {
		t.Fatalf("AddFeatures() error = %v, want an ungrounded feature refused", err)
	}
	sections := completeSections(anyFactID(t, facts))
	sections[0].GroundedIn = nil
	if err := record.AddSections(sections); err == nil || !strings.Contains(err.Error(), "cites no extracted fact") {
		t.Fatalf("AddSections() error = %v, want an ungrounded Launch refused", err)
	}
}

// TestRecord_RejectsABatchWholeOrNotAtAll keeps a partly wrong proposal from
// being split into an accepted prefix and a rejected remainder.
func TestRecord_RejectsABatchWholeOrNotAtAll(t *testing.T) {
	record, facts := newRecordOver(t)
	fact := anyFactID(t, facts)
	good := completeFeature("Baskets", fact)
	bad := completeFeature("Accounts", fact)
	bad.Gotchas = ""
	if err := record.AddFeatures([]Feature{good, bad}); err == nil {
		t.Fatal("AddFeatures() accepted a batch with an invalid feature in it")
	}
	if err := record.AddFeatures([]Feature{good}); err != nil {
		t.Fatalf("AddFeatures() error = %v - the rejected batch should have recorded nothing", err)
	}
}

func TestRecord_RefusesTheSameFeatureTwice(t *testing.T) {
	record, facts := newRecordOver(t)
	fact := anyFactID(t, facts)
	if err := record.AddFeatures([]Feature{completeFeature("Baskets", fact)}); err != nil {
		t.Fatalf("AddFeatures() error = %v", err)
	}
	if err := record.AddFeatures([]Feature{completeFeature("baskets", fact)}); err == nil {
		t.Fatal("AddFeatures() recorded the same feature twice under a different case")
	}
	if err := record.AddFeatures([]Feature{completeFeature("Accounts", fact), completeFeature("Accounts", fact)}); err == nil {
		t.Fatal("AddFeatures() recorded one feature twice within a batch")
	}
}

func TestNewRecord_TakesTheCorpusItValidatesAgainst(t *testing.T) {
	corpus, facts, err := Prepare(newRepo(t, webApp))
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	var _ *documenter.Corpus = corpus
	if record := NewRecord(corpus, facts); record == nil {
		t.Fatal("NewRecord() returned nil")
	}
}
