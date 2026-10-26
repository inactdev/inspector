package guidebook

import (
	"strings"
	"testing"

	"github.com/inactdev/inspector/internal/examiner"
)

func renderFixture(t *testing.T) ([]byte, Guidebook) {
	t.Helper()
	record, facts := newRecordOver(t)
	fact := anyFactID(t, facts)
	if err := record.AddSections(completeSections(fact)); err != nil {
		t.Fatalf("AddSections() error = %v", err)
	}
	if err := record.AddFeatures([]Feature{completeFeature("Baskets", fact)}); err != nil {
		t.Fatalf("AddFeatures() error = %v", err)
	}
	book, err := record.Build("anthropic (default, no flag given)", "test-model")
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	return Render(book), book
}

// TestRender_CarriesEverySectionTheExaminerReads is the document's contract.
func TestRender_CarriesEverySectionTheExaminerReads(t *testing.T) {
	document, _ := renderFixture(t)
	text := string(document)
	previous := -1
	for _, name := range RequiredSections {
		heading := "\n## " + string(name) + "\n"
		at := strings.Index(text, heading)
		if at < 0 {
			t.Fatalf("rendered guidebook has no %q section", name)
		}
		if at < previous {
			t.Errorf("section %q is out of order", name)
		}
		previous = at
	}
	for _, heading := range []string{
		"\n## Features\n", "\n### Baskets\n",
		"\n#### Sub-features\n", "\n#### How to get to it from the user's point of view\n",
		"\n#### Driving it with the harness\n", "\n#### Gotchas\n",
		"\n## Facts Inspector extracted\n",
	} {
		if !strings.Contains(text, heading) {
			t.Errorf("rendered guidebook has no %q heading", strings.TrimSpace(heading))
		}
	}
}

// TestRender_ListsTheFactsVerbatimWithTheirPlaces is what lets a confirming
// reader tell fact from inference without opening the repository.
func TestRender_ListsTheFactsVerbatimWithTheirPlaces(t *testing.T) {
	document, book := renderFixture(t)
	text := string(document)
	for _, fact := range book.Facts.Facts {
		if !strings.Contains(text, fact.Detail) {
			t.Errorf("fact %s (%q) is not quoted in the document", fact.ID, fact.Detail)
		}
		if !strings.Contains(text, fact.Path) {
			t.Errorf("fact %s does not name the file it came from", fact.ID)
		}
	}
	if !strings.Contains(text, "Read from: `"+book.Facts.Facts[0].ID+"`") {
		t.Error("the prose does not say which facts it rests on")
	}
}

func TestRender_RecordsWhoWroteIt(t *testing.T) {
	document, _ := renderFixture(t)
	text := string(document)
	if !strings.Contains(text, "vendor: anthropic (default, no flag given)") || !strings.Contains(text, "model: test-model") {
		t.Error("the front matter does not record the vendor and model, so the choice is silent")
	}
	if !strings.Contains(text, "commit: ") {
		t.Error("the front matter does not say which commit the guidebook describes")
	}
}

// TestRender_IsUnconfirmedAndTheExaminerAgrees is the one-dialect guarantee:
// the renderer writes the confirmation line through the examiner's own
// constants, and the examiner's own parser reads what was written. If these two
// ever drift, a generated draft could be examined as authority.
func TestRender_IsUnconfirmedAndTheExaminerAgrees(t *testing.T) {
	document, _ := renderFixture(t)
	state := examiner.ParseGuidebookConfirmation(document)
	if !state.Present {
		t.Fatal("the examiner does not see a confirmation line in a freshly generated guidebook")
	}
	if state.Confirmed {
		t.Fatal("a freshly generated guidebook reads as confirmed; nothing generated is ever authority")
	}
	if !state.Unconfirmed() {
		t.Fatal("state.Unconfirmed() = false, want a generated draft to stop an examination confirming anything")
	}
}

// TestRender_BecomesConfirmedWhenSomebodySignsIt is the other half: the line a
// person is told to write is a line the examiner accepts.
func TestRender_BecomesConfirmedWhenSomebodySignsIt(t *testing.T) {
	document, _ := renderFixture(t)
	signed := strings.Replace(string(document),
		examiner.GuidebookConfirmedKey+": "+examiner.GuidebookUnconfirmedValue,
		examiner.GuidebookConfirmedKey+": 2026-10-25 Jane Mallory", 1)
	if signed == string(document) {
		t.Fatal("the confirmation line the preamble tells a reader to replace is not the line the document carries")
	}
	state := examiner.ParseGuidebookConfirmation([]byte(signed))
	if !state.Confirmed || state.Date != "2026-10-25" || state.Name != "Jane Mallory" {
		t.Fatalf("ParseGuidebookConfirmation() = %#v, want the signature accepted", state)
	}
}

// TestRender_TellsTheReaderWhatConfirmingMeans - a confirmation nobody
// understands is a signature nobody should trust.
func TestRender_TellsTheReaderWhatConfirmingMeans(t *testing.T) {
	document, _ := renderFixture(t)
	text := string(document)
	for _, phrase := range []string{"draft until somebody confirms it", "could-not-be-tested", "the facts are what happened"} {
		if !strings.Contains(text, phrase) {
			t.Errorf("the document does not say %q", phrase)
		}
	}
}

// TestRender_LeavesNoRunsOfBlankLines - a person has to read this document
// before signing for it, so its layout is part of the deliverable.
func TestRender_LeavesNoRunsOfBlankLines(t *testing.T) {
	document, _ := renderFixture(t)
	if strings.Contains(string(document), "\n\n\n") {
		t.Error("the rendered guidebook contains a run of blank lines")
	}
	if !strings.HasSuffix(string(document), "\n") {
		t.Error("the rendered guidebook does not end with a newline")
	}
}

// TestRender_IsDeterministic keeps a second run diffable against the first.
func TestRender_IsDeterministic(t *testing.T) {
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
	if string(Render(book)) != string(Render(book)) {
		t.Error("two renders of one guidebook differ")
	}
}
