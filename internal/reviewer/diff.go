package reviewer

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// LineKind names what one annotated diff line represents.
type LineKind string

const (
	Context LineKind = "context"
	Added   LineKind = "added"
	Removed LineKind = "removed"
)

// Line is one line of a changed file, carrying whichever line numbers it
// actually has. An added or context line has NewLine; a removed or context
// line has OldLine. A removed line has no NewLine, because it does not exist
// in the file as it now stands.
type Line struct {
	Kind    LineKind
	NewLine int
	OldLine int
	Text    string
}

// File is one changed file's annotated lines. Added and Deleted come from
// the diff's own /dev/null sides rather than from guessing at the lines: a
// pure-deletion hunk generated with no context is indistinguishable from a
// deleted file by line kinds alone.
type File struct {
	Path    string
	Added   bool
	Deleted bool
	Lines   []Line
}

// ParseUnifiedDiff reads a git-style unified diff into per-file line
// records, each carrying the line numbers a finding must point at - the same
// numbers a reviewer reading the diff would see.
func ParseUnifiedDiff(diff []byte) ([]File, error) {
	var files []File
	var current *File
	var pendingOldPath string
	var pendingAdded bool
	var newLine, oldLine int
	inHunk := false

	scanner := bufio.NewScanner(bytes.NewReader(diff))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "--- "):
			// The file only starts on the matching "+++" line below, so a
			// deleted file (whose "+++" side is /dev/null) still gets its
			// one real path from here instead of a second, empty entry.
			pendingOldPath = oldFilePath(line)
			pendingAdded = pendingOldPath == ""
			current = nil
			inHunk = false
		case strings.HasPrefix(line, "+++ "):
			path := newFilePath(line)
			deleted := path == ""
			if deleted {
				path = pendingOldPath
			}
			if path == "" {
				current = nil
				inHunk = false
				continue
			}
			files = append(files, File{Path: path, Added: pendingAdded, Deleted: deleted})
			current = &files[len(files)-1]
			inHunk = false
		case strings.HasPrefix(line, "@@ "):
			old, new, err := parseHunkHeader(line)
			if err != nil {
				return nil, err
			}
			oldLine, newLine = old, new
			inHunk = true
		case current != nil && inHunk && strings.HasPrefix(line, "\\"):
			// "\ No newline at end of file" - no counters change.
		case current != nil && inHunk && strings.HasPrefix(line, " "):
			current.Lines = append(current.Lines, Line{Kind: Context, NewLine: newLine, OldLine: oldLine, Text: line[1:]})
			newLine++
			oldLine++
		case current != nil && inHunk && strings.HasPrefix(line, "+"):
			current.Lines = append(current.Lines, Line{Kind: Added, NewLine: newLine, Text: line[1:]})
			newLine++
		case current != nil && inHunk && strings.HasPrefix(line, "-"):
			current.Lines = append(current.Lines, Line{Kind: Removed, OldLine: oldLine, Text: line[1:]})
			oldLine++
		default:
			// "diff --git", "index ...", mode changes, and similar framing
			// lines carry nothing an annotated review needs.
			inHunk = false
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading diff: %w", err)
	}
	return files, nil
}

func newFilePath(line string) string {
	path := strings.TrimSpace(strings.TrimPrefix(line, "+++ "))
	if path == "/dev/null" {
		return ""
	}
	return trimDiffPathPrefix(path, "b/")
}

func oldFilePath(line string) string {
	path := strings.TrimSpace(strings.TrimPrefix(line, "--- "))
	if path == "/dev/null" {
		return ""
	}
	return trimDiffPathPrefix(path, "a/")
}

func trimDiffPathPrefix(path, prefix string) string {
	if tab := strings.IndexByte(path, '\t'); tab >= 0 {
		path = path[:tab]
	}
	return strings.TrimPrefix(path, prefix)
}

// parseHunkHeader reads "@@ -oldStart[,oldLines] +newStart[,newLines] @@...".
func parseHunkHeader(line string) (oldStart, newStart int, err error) {
	rest := strings.TrimPrefix(line, "@@ ")
	closing := strings.Index(rest, " @@")
	if closing < 0 {
		return 0, 0, fmt.Errorf("malformed hunk header %q", line)
	}
	fields := strings.Fields(rest[:closing])
	if len(fields) != 2 || !strings.HasPrefix(fields[0], "-") || !strings.HasPrefix(fields[1], "+") {
		return 0, 0, fmt.Errorf("malformed hunk header %q", line)
	}
	oldStart, err = firstNumber(fields[0][1:])
	if err != nil {
		return 0, 0, fmt.Errorf("malformed hunk header %q: %w", line, err)
	}
	newStart, err = firstNumber(fields[1][1:])
	if err != nil {
		return 0, 0, fmt.Errorf("malformed hunk header %q: %w", line, err)
	}
	return oldStart, newStart, nil
}

func firstNumber(field string) (int, error) {
	if comma := strings.IndexByte(field, ','); comma >= 0 {
		field = field[:comma]
	}
	return strconv.Atoi(field)
}

// RenderForReview formats parsed diff files into the annotated text given to
// the model: a left-column line number it can cite directly, so a finding's
// pointer never depends on the model's own diff arithmetic.
func RenderForReview(files []File) string {
	var b strings.Builder
	for _, file := range files {
		fmt.Fprintf(&b, "FILE %s\n", file.Path)
		for _, l := range file.Lines {
			switch l.Kind {
			case Context:
				fmt.Fprintf(&b, "%6d   %s\n", l.NewLine, l.Text)
			case Added:
				fmt.Fprintf(&b, "%6d + %s\n", l.NewLine, l.Text)
			case Removed:
				fmt.Fprintf(&b, "       - (removed, was old line %d; cite the nearest shown line instead) %s\n", l.OldLine, l.Text)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// Paths returns the set of file paths present in the parsed change, used to
// reject a finding naming a file outside it.
func Paths(files []File) map[string]struct{} {
	set := make(map[string]struct{}, len(files))
	for _, f := range files {
		set[f.Path] = struct{}{}
	}
	return set
}
