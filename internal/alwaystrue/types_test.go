package alwaystrue

import (
	"strings"
	"testing"
)

// A broken promise outranks incompleteness, and neither hides the other: the
// aggregate is red, and the fact that part of the list was never checked still
// travels with it.
func TestDeriveResult_ABrokenPromiseOutranksButDoesNotHideIncompleteness(t *testing.T) {
	result := DeriveResult("list.json", true, "", []PromiseResult{
		{ID: "held", Outcome: Held, Confirmed: true},
		{ID: "broke", Outcome: Broken, Reason: "answered 500"},
		{ID: "unrunnable", Outcome: NotRunnable},
	})
	if result.Kind != Red {
		t.Fatalf("kind = %q, want %q", result.Kind, Red)
	}
	if !result.Incomplete {
		t.Fatal("a not-runnable line must leave the result incomplete")
	}
	summary := result.Summary()
	if !strings.Contains(summary, "broke") || !strings.Contains(summary, "unrunnable") {
		t.Fatalf("summary = %q, want both the broken and the unchecked line named", summary)
	}
}

// Nothing broke, but nothing was checked either. That is an error state, not a
// green one - the whole point of reporting not-runnable separately.
func TestDeriveResult_NotRunnableIsNeverAPass(t *testing.T) {
	result := DeriveResult("list.json", false, "", []PromiseResult{
		{ID: "a", Outcome: NotRunnable},
		{ID: "b", Outcome: NotRunnable},
	})
	if result.Kind != Green {
		t.Fatalf("kind = %q, want %q - nothing broke", result.Kind, Green)
	}
	if !result.Incomplete {
		t.Fatal("a list where nothing ran must not report as fully checked")
	}
	if strings.Contains(result.Summary(), "still hold") {
		t.Fatalf("summary = %q, must not read as everything holding", result.Summary())
	}
	if !strings.Contains(result.Summary(), "2 promise(s) not checked: a, b") {
		t.Fatalf("summary = %q, want it to say which lines were not checked", result.Summary())
	}
}

func TestDeriveResult_AnAppThatWouldNotStartIsIncompleteAndNamed(t *testing.T) {
	result := DeriveResult("list.json", false, "the app would not start: no such image", []PromiseResult{
		{ID: "a", Outcome: NotRun},
	})
	if result.Kind != Green || !result.Incomplete {
		t.Fatalf("kind = %q incomplete = %v, want green and incomplete", result.Kind, result.Incomplete)
	}
	if !strings.Contains(result.Summary(), "no such image") {
		t.Fatalf("summary = %q, want the infrastructure failure named", result.Summary())
	}
}

func TestDeriveResult_EveryPromiseHeld(t *testing.T) {
	result := DeriveResult("list.json", true, "", []PromiseResult{
		{ID: "a", Outcome: Held, Confirmed: true},
		{ID: "b", Outcome: Held, Confirmed: true},
	})
	if result.Kind != Green || result.Incomplete {
		t.Fatalf("kind = %q incomplete = %v, want a complete green", result.Kind, result.Incomplete)
	}
	if result.Summary() != "all 2 promise(s) still hold" {
		t.Fatalf("summary = %q", result.Summary())
	}
}

// An outcome nobody set cannot read as one that held. Without this, a new code
// path that forgot to record an outcome would silently widen what passes.
func TestDeriveResult_AnUnsetOutcomeIsNotAPass(t *testing.T) {
	result := DeriveResult("list.json", true, "", []PromiseResult{{ID: "a"}})
	if !result.Incomplete {
		t.Fatal("a promise with no recorded outcome must leave the result incomplete")
	}
}

// Confirmation is reported, never a filter: a promise nobody accepted yet is
// still a promise that broke.
func TestUnconfirmedIsCountedWithoutChangingTheVerdict(t *testing.T) {
	result := DeriveResult("list.json", true, "", []PromiseResult{
		{ID: "proposal", Outcome: Broken, Confirmed: false},
		{ID: "accepted", Outcome: Held, Confirmed: true},
	})
	if result.Kind != Red {
		t.Fatalf("kind = %q, want %q", result.Kind, Red)
	}
	if result.Unconfirmed() != 1 {
		t.Fatalf("unconfirmed = %d, want 1", result.Unconfirmed())
	}
}

func TestSummary_NamesEveryBrokenPromiseRatherThanTheFirst(t *testing.T) {
	result := DeriveResult("list.json", true, "", []PromiseResult{
		{ID: "first", Outcome: Broken},
		{ID: "second", Outcome: Broken},
		{ID: "third", Outcome: Broken},
	})
	summary := result.Summary()
	for _, want := range []string{"3 promise(s) broken", "first", "second", "third"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary = %q, want %q in it", summary, want)
		}
	}
}

// The posted status has 140 characters, so it leads with the counts and ids a
// reader acts on and keeps only the headline of a long failure message. The
// full text stays in the record.
func TestStatusDescription_IsCompactAndStillNamesBothFacts(t *testing.T) {
	result := DeriveResult("list.json", false, "the app would not start: starting app:local: Error response from daemon: No such image", []PromiseResult{
		{ID: "broke", Outcome: Broken},
		{ID: "unchecked", Outcome: NotRun},
	})
	description := result.StatusDescription()
	for _, want := range []string{"1 of 2 promise(s) broken: broke", "the app would not start", "unchecked"} {
		if !strings.Contains(description, want) {
			t.Errorf("description = %q, want %q in it", description, want)
		}
	}
	if strings.Contains(description, "No such image") {
		t.Errorf("description = %q, want only the failure headline; the full message belongs in the record", description)
	}
	if len(description) > 140 {
		t.Errorf("description is %d characters, more than a commit status holds: %q", len(description), description)
	}
}

func TestShapeCounts_ReportsWhatTheListIsMadeOf(t *testing.T) {
	result := DeriveResult("list.json", true, "", []PromiseResult{
		{ID: "a", Shape: "http", Outcome: Held},
		{ID: "b", Shape: "http", Outcome: Held},
		{ID: "c", Shape: "none", Outcome: NotRunnable},
	})
	counts := strings.Join(result.ShapeCounts(), ", ")
	if counts != "http: 2, none: 1" {
		t.Fatalf("shape counts = %q", counts)
	}
}

func TestRefusedResult_IsIncompleteAndCarriesItsReason(t *testing.T) {
	result := RefusedResult("list.json", "no such file")
	if result.Kind != Refused || !result.Incomplete {
		t.Fatalf("result = %#v, want a refusal that is also incomplete", result)
	}
	if !strings.Contains(result.Summary(), "no such file") {
		t.Fatalf("summary = %q, want the reason", result.Summary())
	}
}
