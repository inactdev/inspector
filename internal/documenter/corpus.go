package documenter

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// Caps on what one run may read. Each one is named in the corpus summary the
// model and the Client both see, rather than silently shrinking the corpus:
// a documentation judgment that skipped half the project must not read like
// one that found nothing.
const (
	// maxFileBytes bounds one corpus file. Anything larger is not prose a
	// person reads as a claim - it is generated output, a lock file, or data.
	maxFileBytes = 512 * 1024
	// maxCorpusBytes bounds the whole corpus held in memory at once. It is
	// generous for documentation and deliberately well short of what a
	// repository can hold: the corpus is kept split into lines, so a budget
	// set by raw bytes costs noticeably more than its own size in memory.
	maxCorpusBytes = 16 * 1024 * 1024
	// maxListedPaths bounds the up-front path listing. Past this the model is
	// told the count and pointed at list_files instead.
	maxListedPaths = 400
	// maxSearchMatches bounds one search_project result.
	maxSearchMatches = 60
	// maxSearchLineWidth bounds one match's quoted text.
	maxSearchLineWidth = 240
	// maxReadLines bounds one read_file result; the tool takes a start line,
	// so a longer file is read in successive calls rather than truncated away.
	maxReadLines = 600
	// claimLineSlack is how far a cited line may sit from where the quoted
	// claim actually starts. A claim often spans lines and a pointer at its
	// heading rather than its first word is still a usable pointer; being out
	// by more than this is a pointer nobody can follow.
	claimLineSlack = 2
)

// SkipReason names why one tracked file is not in the corpus.
type SkipReason string

const (
	SkippedBinary     SkipReason = "not text, so nobody reads it as a claim"
	SkippedTooLarge   SkipReason = "larger than the per-file cap"
	SkippedUnreadable SkipReason = "could not be read"
	SkippedNotRegular SkipReason = "not a regular file"
	SkippedCorpusFull SkipReason = "the corpus byte budget was already full"
)

// SkippedFile is one tracked file left out of the corpus and the reason.
type SkippedFile struct {
	Path   string     `json:"path"`
	Reason SkipReason `json:"reason"`
}

// CorpusSummary is what the run reports about the documentation it was able to
// read. It travels in the result so a judgment made over a partial corpus can
// never be mistaken for one made over all of it.
type CorpusSummary struct {
	Root    string `json:"root"`
	Files   int    `json:"files"`
	Skipped int    `json:"skipped,omitempty"`
	// SkippedExamples names up to ten of the skipped files with their
	// reasons. The full list is deliberately not carried: a repository of
	// images would bury the finding it was printed beside.
	SkippedExamples []SkippedFile `json:"skippedExamples,omitempty"`
}

// corpusFile is one readable file, split into lines once so every tool and
// every claim check reads the same text.
type corpusFile struct {
	path  string
	lines []string
}

// Corpus is the project's committed, human-readable text as the working tree
// now stands: the places a claim about behavior can live, which is why it is
// deliberately not limited to prose files.
//
// It is computed in full before the first model call, and the read, search,
// and list tools serve only from it. The model therefore never chooses which
// bytes inspector opens - an untracked file, an ignored one, or a tracked
// symlink pointing out of the tree is not in the set to begin with.
type Corpus struct {
	root  string
	files []corpusFile
	index map[string]int

	skipped []SkippedFile
}

// LoadCorpus reads the tracked text files of the git working tree containing
// repoPath. Asking git rather than walking the filesystem is the point: an
// untracked or ignored file is not a claim the project publishes, and it is
// where a credential lives.
func LoadCorpus(repoPath string) (*Corpus, error) {
	root, err := repoRoot(repoPath)
	if err != nil {
		return nil, err
	}
	paths, err := trackedPaths(root)
	if err != nil {
		return nil, err
	}

	corpus := &Corpus{root: root, index: make(map[string]int, len(paths))}
	budget := maxCorpusBytes
	for _, path := range paths {
		absolute := filepath.Join(root, filepath.FromSlash(path))
		info, err := os.Lstat(absolute)
		switch {
		case err != nil:
			corpus.skip(path, SkippedUnreadable)
			continue
		case !info.Mode().IsRegular():
			// Covers a tracked symlink, whose target can sit anywhere on the
			// host; the corpus is files of this tree, not whatever they point at.
			corpus.skip(path, SkippedNotRegular)
			continue
		case info.Size() > maxFileBytes:
			corpus.skip(path, SkippedTooLarge)
			continue
		case int(info.Size()) > budget:
			corpus.skip(path, SkippedCorpusFull)
			continue
		}
		content, err := os.ReadFile(absolute)
		if err != nil {
			corpus.skip(path, SkippedUnreadable)
			continue
		}
		if !isText(content) {
			corpus.skip(path, SkippedBinary)
			continue
		}
		budget -= len(content)
		corpus.index[path] = len(corpus.files)
		corpus.files = append(corpus.files, corpusFile{path: path, lines: splitLines(content)})
	}
	if len(corpus.files) == 0 {
		return nil, fmt.Errorf("found no readable tracked text file under %s, so there is no documentation to re-read; asking whether the change falsified something would report nothing found when nothing was looked at", root)
	}
	return corpus, nil
}

// isText is a lighter rule than agentsource.firstNonTextByte's, on purpose.
// That one must refuse anything unreviewable under the agent source tree; this
// one only decides whether a file is prose somebody could read a claim in, and
// skipping a file is not a verdict about it.
func isText(content []byte) bool {
	return utf8.Valid(content) && !bytes.ContainsRune(content, 0)
}

func splitLines(content []byte) []string {
	text := strings.ReplaceAll(string(content), "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func (c *Corpus) skip(path string, reason SkipReason) {
	c.skipped = append(c.skipped, SkippedFile{Path: path, Reason: reason})
}

// Root is the absolute working-tree root every corpus path is relative to.
func (c *Corpus) Root() string { return c.root }

// Has reports whether path is a file in the corpus. It is what makes "the
// project needs a document it never had" an unrecordable finding: a finding
// may only name something already written (SPEC.md section 4).
func (c *Corpus) Has(path string) bool {
	_, ok := c.index[path]
	return ok
}

// Summary is what this run read, and what it did not.
func (c *Corpus) Summary() CorpusSummary {
	summary := CorpusSummary{Root: c.root, Files: len(c.files), Skipped: len(c.skipped)}
	for i, skipped := range c.skipped {
		if i == 10 {
			break
		}
		summary.SkippedExamples = append(summary.SkippedExamples, skipped)
	}
	return summary
}

// Describe is the corpus as the model is told about it: how much was read, what
// was skipped and why, and - for a project small enough that the whole list is
// cheaper than a search - every path in it.
func (c *Corpus) Describe() string {
	var b strings.Builder
	b.WriteString("PROJECT DOCUMENTATION CORPUS (inspector built this list itself from the project's tracked files; read_file, search_project and list_files serve only from it):\n\n")
	fmt.Fprintf(&b, "  %d readable text files", len(c.files))
	if len(c.skipped) > 0 {
		fmt.Fprintf(&b, ", and %d tracked files left out", len(c.skipped))
	}
	b.WriteString("\n")
	for i, skipped := range c.skipped {
		if i == 10 {
			fmt.Fprintf(&b, "    ... and %d more left out\n", len(c.skipped)-10)
			break
		}
		fmt.Fprintf(&b, "    %s: %s\n", skipped.Path, skipped.Reason)
	}
	b.WriteString("\n")
	if len(c.files) <= maxListedPaths {
		b.WriteString("Every file in the corpus:\n")
		for _, file := range c.files {
			fmt.Fprintf(&b, "  %s\n", file.path)
		}
	} else {
		fmt.Fprintf(&b, "Too many files (%d) to list here; use list_files to see them by prefix, or search_project to go straight to a claim.\n", len(c.files))
	}
	b.WriteString("\n")
	return b.String()
}

// List renders corpus paths under prefix, in corpus order, saying outright when
// it stopped short rather than letting the cap read as the whole answer.
func (c *Corpus) List(prefix string) string {
	var matched []string
	for _, file := range c.files {
		if prefix == "" || strings.HasPrefix(file.path, prefix) {
			matched = append(matched, file.path)
		}
	}
	if len(matched) == 0 {
		return fmt.Sprintf("no corpus file has the prefix %q", prefix)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d file(s) with prefix %q:\n", len(matched), prefix)
	for i, path := range matched {
		if i == maxListedPaths {
			fmt.Fprintf(&b, "... %d more not listed; narrow the prefix\n", len(matched)-maxListedPaths)
			break
		}
		fmt.Fprintf(&b, "  %s\n", path)
	}
	return b.String()
}

// Search is a literal, case-insensitive substring search across the corpus.
// Literal rather than regular-expression on purpose: one matching dialect the
// caller can predict, and no pattern that can cost the run its time budget.
func (c *Corpus) Search(query string) string {
	query = strings.TrimSpace(query)
	if query == "" {
		return "rejected: search_project needs a non-empty query"
	}
	needle := strings.ToLower(query)
	var b strings.Builder
	matches, truncated := 0, false
	for _, file := range c.files {
		for number, line := range file.lines {
			if !strings.Contains(strings.ToLower(line), needle) {
				continue
			}
			if matches == maxSearchMatches {
				truncated = true
				break
			}
			matches++
			fmt.Fprintf(&b, "%s:%d: %s\n", file.path, number+1, truncate(strings.TrimSpace(line), maxSearchLineWidth))
		}
		if truncated {
			break
		}
	}
	if matches == 0 {
		return fmt.Sprintf("no corpus file contains %q", query)
	}
	header := fmt.Sprintf("%d match(es) for %q:\n", matches, query)
	if truncated {
		header = fmt.Sprintf("the first %d match(es) for %q; there are more, so narrow the query before concluding anything from the absence of a hit:\n", matches, query)
	}
	return header + b.String()
}

// Read renders one corpus file from fromLine, each line carrying the number a
// finding must cite - the same annotation the code reviewer gives its diff, so
// a pointer never depends on the model's own counting.
func (c *Corpus) Read(path string, fromLine int) string {
	position, ok := c.index[path]
	if !ok {
		return fmt.Sprintf("rejected: %q is not a file in the corpus; use list_files or search_project to find the path as inspector has it", path)
	}
	file := c.files[position]
	if fromLine <= 0 {
		fromLine = 1
	}
	if fromLine > len(file.lines) {
		return fmt.Sprintf("%s has %d line(s), so there is nothing at line %d", path, len(file.lines), fromLine)
	}
	last := fromLine + maxReadLines - 1
	if last > len(file.lines) {
		last = len(file.lines)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "FILE %s (lines %d-%d of %d)\n", path, fromLine, last, len(file.lines))
	for number := fromLine; number <= last; number++ {
		fmt.Fprintf(&b, "%6d  %s\n", number, file.lines[number-1])
	}
	if last < len(file.lines) {
		fmt.Fprintf(&b, "... %d more line(s); call read_file again with from_line %d\n", len(file.lines)-last, last+1)
	}
	return b.String()
}

// VerifyClaim requires that quote really is what path says at or near line.
//
// The documenter chooses what it reads, so unlike the code reviewer's diff
// inspector has not already shown the model the text a finding cites. Making
// the finding bring the claim and checking it back against the file is what
// keeps a confidently-worded invention out of the record. Whitespace is
// collapsed before comparing, because a claim quoted out of wrapped prose or a
// re-indented comment is still the same claim.
func (c *Corpus) VerifyClaim(path string, line int, quote string) error {
	position, ok := c.index[path]
	if !ok {
		return fmt.Errorf("%q is not a file in the corpus", path)
	}
	file := c.files[position]
	if line <= 0 || line > len(file.lines) {
		return fmt.Errorf("%s has %d line(s), so line %d is not a place in it", path, len(file.lines), line)
	}
	normalizedQuote := normalizeSpace(quote)
	if normalizedQuote == "" {
		return fmt.Errorf("the quoted claim for %s:%d is empty", path, line)
	}
	span := len(strings.Split(strings.TrimSpace(quote), "\n"))
	start := line - 1 - claimLineSlack
	if start < 0 {
		start = 0
	}
	end := line - 1 + span + claimLineSlack
	if end > len(file.lines) {
		end = len(file.lines)
	}
	if strings.Contains(normalizeSpace(strings.Join(file.lines[start:end], "\n")), normalizedQuote) {
		return nil
	}
	if strings.Contains(normalizeSpace(strings.Join(file.lines, "\n")), normalizedQuote) {
		return fmt.Errorf("the quoted claim is in %s but not at line %d; point the finding at the line the claim is actually on", path, line)
	}
	return fmt.Errorf("%s does not contain the quoted claim %q anywhere, so there is nothing at %s:%d to correct", path, truncate(normalizedQuote, 120), path, line)
}

func normalizeSpace(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "..."
}

func repoRoot(path string) (string, error) {
	out, err := runGit(path, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("%s is not inside a git repository, so inspector cannot tell which files the project actually publishes: %w", path, err)
	}
	return strings.TrimSpace(out), nil
}

func trackedPaths(root string) ([]string, error) {
	out, err := runGit(root, "ls-files", "-z")
	if err != nil {
		return nil, fmt.Errorf("listing the project's tracked files: %w", err)
	}
	seen := make(map[string]struct{})
	var paths []string
	for _, path := range strings.Split(out, "\x00") {
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			// An unmerged path is listed once per stage.
			continue
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("%s has no tracked files", root)
	}
	sort.Strings(paths)
	return paths, nil
}

func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, message)
	}
	return stdout.String(), nil
}
