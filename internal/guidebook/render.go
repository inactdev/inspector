package guidebook

import (
	"fmt"
	"sort"
	"strings"

	"github.com/inactdev/inspector/internal/examiner"
)

// Render turns the validated guidebook into the markdown file the examiner is
// given. Inspector renders it, not the model: the section order, the four
// per-feature headings, the front matter and the facts appendix are the
// document's contract with the examiner, and a model that could lay the file
// out itself could quietly drop a section.
//
// The output is deterministic - the same commit and the same validated content
// render byte for byte identically - so a second run produces a draft somebody
// can diff against what is already there.
func Render(book Guidebook) []byte {
	var b strings.Builder
	renderFrontMatter(&b, book)
	fmt.Fprintf(&b, "# %s: the examiner's guidebook\n\n", book.Facts.Project)
	b.WriteString(preamble)
	for _, name := range RequiredSections {
		section := book.Sections[name]
		if section == nil {
			continue
		}
		fmt.Fprintf(&b, "## %s\n\n%s\n\n", name, strings.TrimSpace(section.Body))
		renderGrounding(&b, section.GroundedIn, book.Facts)
	}
	b.WriteString("## Features\n\n")
	b.WriteString("One entry per user-facing feature, each with the same four headings.\n\n")
	for _, feature := range book.Features {
		fmt.Fprintf(&b, "### %s\n\n", feature.Name)
		for _, heading := range []struct{ title, body string }{
			{"Sub-features", feature.SubFeatures},
			{"How to get to it from the user's point of view", feature.HowToReachIt},
			{"Driving it with the harness", feature.DrivingIt},
			{"Gotchas", feature.Gotchas},
		} {
			fmt.Fprintf(&b, "#### %s\n\n%s\n\n", heading.title, strings.TrimSpace(heading.body))
		}
		renderGrounding(&b, feature.GroundedIn, book.Facts)
	}
	b.WriteString("## Facts Inspector extracted\n\n")
	b.WriteString(book.Facts.Render())
	return []byte(b.String())
}

// renderFrontMatter writes the document's own header. The confirmed line is the
// one the examiner reads, so it is written through the examiner's own constants
// rather than spelled out here: one parser, one producer, no second dialect of
// the fact that decides whether this file is authority.
func renderFrontMatter(b *strings.Builder, book Guidebook) {
	b.WriteString("---\n")
	fmt.Fprintf(b, "project: %s\n", book.Facts.Project)
	if book.Facts.Commit != "" {
		// The commit rather than a timestamp: a guidebook says which version of
		// the project it describes, and the same commit regenerates to the same
		// bytes. The date that matters is the one in the confirmed line, and a
		// person puts it there.
		fmt.Fprintf(b, "commit: %s\n", book.Facts.Commit)
	}
	fmt.Fprintf(b, "vendor: %s\n", orNone(book.Vendor))
	fmt.Fprintf(b, "model: %s\n", orNone(book.Model))
	fmt.Fprintf(b, "%s: %s\n", examiner.GuidebookConfirmedKey, examiner.GuidebookUnconfirmedValue)
	b.WriteString("---\n\n")
}

const preamble = `This file teaches Inspector's examiner how to operate this project. The
examiner is given this document and a running app, and never the source - so
anything it needs to know in order to drive a feature has to be in here.

**This guidebook is a draft until somebody confirms it.** Follow every command
below yourself, correct what is wrong, then replace the ` + "`" + `confirmed` + "`" + ` line in the
front matter with today's date and your name. Until that line names a date and
a person, an examination driven from this file records every capability as
could-not-be-tested rather than as a pass: nobody has vouched for these
instructions, so a response that arrives proves only that this draft's own
scenario ran.

The prose below is a model's reading of the facts in the last section. Those
facts were read out of the project's own files by Inspector, each with the line
it came from. When the two disagree, the facts are what happened.

`

// renderGrounding prints which extracted facts a piece of prose rests on, right
// under that prose. Inline rather than collected at the end: a reader checking
// one instruction should not have to work out which facts were behind it.
//
// Every caller has already ended its block with a blank line, so this adds none
// of its own and writes nothing at all when there is nothing to cite.
func renderGrounding(b *strings.Builder, ids []string, facts Facts) {
	if len(ids) == 0 {
		return
	}
	places := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		fact, ok := facts.Lookup(id)
		if !ok {
			continue
		}
		place := fmt.Sprintf("`%s` (%s:%d)", id, fact.Path, fact.Line)
		if _, exists := seen[place]; exists {
			continue
		}
		seen[place] = struct{}{}
		places = append(places, place)
	}
	if len(places) == 0 {
		return
	}
	sort.Strings(places)
	fmt.Fprintf(b, "Read from: %s\n\n", strings.Join(places, ", "))
}

func orNone(value string) string {
	if strings.TrimSpace(value) == "" {
		return "(not recorded)"
	}
	return value
}
