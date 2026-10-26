package examiner

import (
	"fmt"
	"strings"
	"time"
	"unicode"
)

// GuidebookConfirmedKey is the front-matter key a generated guidebook
// (`inspector guidebook`, issue #22) carries, and the one line that decides
// whether this examination may confirm anything.
//
// The contract lives here, in the consumer, rather than in the generator:
// the examiner's sealed agent derives the verdict, so the parser has to be in
// the agent's own package, and a second parser in the generator would be a
// second dialect of the one fact that decides whether a guidebook is
// authority. internal/guidebook renders what this file parses.
const GuidebookConfirmedKey = "confirmed"

// GuidebookUnconfirmedValue is what a freshly generated guidebook says until a
// human replaces the whole line. It is deliberately a sentence rather than
// "no": the person who has to act on it reads the file, not this constant.
const GuidebookUnconfirmedValue = `no - replace this line with "` + GuidebookConfirmedKey +
	`: YYYY-MM-DD Your Name" once you have followed every command below yourself`

// guidebookPlaceholderName is the name in GuidebookUnconfirmedValue's own
// example. Copying the example line and dating it is the single most likely way
// to produce a confirmation nobody actually made, so this one exact string is
// refused - one string, never a pattern of names that look placeholder-ish,
// which is the kind of rule that fails open on the first form nobody foresaw.
const guidebookPlaceholderName = "your name"

// guidebookHeadLines bounds how far into a document the confirmation line is
// looked for. Front matter sits at the top; a "confirmed:" line in the middle
// of a long feature map is prose, not a signature.
const guidebookHeadLines = 100

// GuidebookConfirmation is what the examiner knows about the guidebook driving
// it. Present and Confirmed are separate facts: a hand-written feature map
// somebody wrote and committed themselves is not an unconfirmed draft, and a
// run must never report one as the other.
type GuidebookConfirmation struct {
	// Present means this document carries a guidebook confirmation line, so it
	// is a generated guidebook whose confirmation state governs the run.
	Present bool
	// Confirmed means that line names a real date and a name.
	Confirmed bool
	Date      string
	Name      string
	// Reason says why an unconfirmed document is unconfirmed, or why a
	// document with no confirmation line was treated as hand-written. It is
	// always set, because an absence that goes unmentioned reads as approval.
	Reason string
}

// ParseGuidebookConfirmation reads the confirmation state of a feature map.
//
// It looks for the confirmation line in the document's head whether or not the
// front matter around it is well formed, so a generated guidebook that lost its
// closing marker cannot be mistaken for a hand-written document - that
// direction is the one that would let an unconfirmed draft produce a pass. A
// document with no such line anywhere in its head, and no closed front matter
// block either, is a hand-written feature map and keeps the behavior it had
// before guidebooks existed.
func ParseGuidebookConfirmation(document []byte) GuidebookConfirmation {
	lines := strings.Split(strings.ReplaceAll(string(document), "\r\n", "\n"), "\n")
	if len(lines) > guidebookHeadLines {
		lines = lines[:guidebookHeadLines]
	}
	var values []string
	for _, line := range lines {
		value, ok := guidebookConfirmedValue(line)
		if ok {
			values = append(values, value)
		}
	}
	switch len(values) {
	case 0:
		if hasClosedFrontMatter(lines) {
			return GuidebookConfirmation{
				Present: true,
				Reason:  fmt.Sprintf("its front matter carries no %q line, so nobody has signed for these instructions", GuidebookConfirmedKey),
			}
		}
		return GuidebookConfirmation{
			Reason: fmt.Sprintf("it carries no %q line, so it is read as a feature map a human wrote and committed rather than a generated draft", GuidebookConfirmedKey),
		}
	case 1:
		return judgeGuidebookConfirmation(values[0])
	default:
		// Refused rather than resolved by picking one: two signatures mean
		// nobody can say which one the run was judged under.
		return GuidebookConfirmation{
			Present: true,
			Reason:  fmt.Sprintf("it carries %d %q lines, so which signature governs this run cannot be established", len(values), GuidebookConfirmedKey),
		}
	}
}

// judgeGuidebookConfirmation requires a real calendar date and a name. Both,
// because a date alone says when somebody looked without saying who, and a name
// alone says who without saying whether it was this version they looked at.
func judgeGuidebookConfirmation(value string) GuidebookConfirmation {
	value = strings.TrimSpace(unquote(strings.TrimSpace(value)))
	if value == "" {
		return GuidebookConfirmation{Present: true, Reason: fmt.Sprintf("its %q line is empty", GuidebookConfirmedKey)}
	}
	date, name, found := strings.Cut(value, " ")
	if !found {
		return GuidebookConfirmation{Present: true, Reason: fmt.Sprintf("its %q line says %q, which names neither a date nor a person", GuidebookConfirmedKey, value)}
	}
	if _, err := time.Parse(time.DateOnly, date); err != nil {
		return GuidebookConfirmation{Present: true, Reason: fmt.Sprintf("its %q line starts with %q rather than a YYYY-MM-DD date", GuidebookConfirmedKey, date)}
	}
	name = strings.TrimSpace(name)
	if !hasLetterOrDigit(name) {
		return GuidebookConfirmation{Present: true, Reason: fmt.Sprintf("its %q line is dated %s but names nobody", GuidebookConfirmedKey, date)}
	}
	if strings.EqualFold(name, guidebookPlaceholderName) {
		return GuidebookConfirmation{Present: true, Reason: fmt.Sprintf("its %q line is dated %s but still names the example's own placeholder %q", GuidebookConfirmedKey, date, name)}
	}
	return GuidebookConfirmation{Present: true, Confirmed: true, Date: date, Name: name}
}

// Describe is the confirmation state as every report and printed line says it,
// so the same words reach the Client wherever he reads them.
func (c GuidebookConfirmation) Describe() string {
	switch {
	case c.Confirmed:
		return fmt.Sprintf("confirmed on %s by %s", c.Date, c.Name)
	case c.Present:
		return "UNCONFIRMED guidebook: " + c.Reason
	default:
		return "no guidebook confirmation line: " + c.Reason
	}
}

// Unconfirmed reports whether this document is a guidebook nobody has signed.
// It is the one condition that stops an examination confirming anything.
func (c GuidebookConfirmation) Unconfirmed() bool { return c.Present && !c.Confirmed }

func guidebookConfirmedValue(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	prefix := GuidebookConfirmedKey + ":"
	if !strings.HasPrefix(trimmed, prefix) {
		return "", false
	}
	return strings.TrimPrefix(trimmed, prefix), true
}

// hasClosedFrontMatter reports whether the document opens with a --- block that
// closes again, which is what makes a document look generated even when its
// confirmation line was deleted outright.
func hasClosedFrontMatter(lines []string) bool {
	start := -1
	for index, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.TrimSpace(line) == "---" {
			start = index
		}
		break
	}
	if start < 0 {
		return false
	}
	for _, line := range lines[start+1:] {
		if strings.TrimSpace(line) == "---" {
			return true
		}
	}
	return false
}

func unquote(value string) string {
	if len(value) < 2 {
		return value
	}
	first, last := value[0], value[len(value)-1]
	if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
		return value[1 : len(value)-1]
	}
	return value
}

func hasLetterOrDigit(value string) bool {
	for _, character := range value {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			return true
		}
	}
	return false
}
