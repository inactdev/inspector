// Package agentsource holds one rule about Inspector's agent source tree: no
// file under it may contain binary content.
//
// The reason is worth keeping, because the code it replaced does not preserve
// it. The first trusted runtime committed two gzipped Linux agent binaries with
// their checksums as constants and a script to prove the checksums matched; it
// was replaced by internal/examiner/runtime/Dockerfile, which builds the agent
// from source. A committed binary is unreviewable by construction - a reviewer
// looking at a .gz learns nothing - and this is a public repository whose agent
// is the component whose entire job is refusing to trust what it cannot verify.
// Nothing about the Dockerfile stops a binary coming back, and a ruling in
// conversation plus a note in a README both already failed to hold once. So the
// rule lives here, where the project's own check command runs it (issue #36).
package agentsource

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// Roots is the agent source tree, named exactly rather than inferred. The
// first three are the directories internal/examiner/runtime/Dockerfile copies
// into the agent image's build context - including internal/examiner/runtime
// itself, where the gzipped binaries were - and the fourth is in the agent's
// own import graph, since internal/examiner/agent.go asks a model through it.
//
// Directories, not files: the rule exists because a new unreviewable file can
// appear in a directory without anyone looking at its bytes. The two
// individually named files that Dockerfile also copies, go.mod and
// runtime_identity.go, are Go-parsed text whose content the toolchain already
// constrains.
var Roots = []string{
	"cmd/examiner-agent",
	"internal/container",
	"internal/examiner",
	"internal/llm",
}

// Allowed is the complete set of paths permitted to hold binary content, each
// mapped to the reason it is permitted. It is empty today, and it is the only
// way to carry a binary file here: a test fixture may genuinely need binary
// content, so the rule has one explicit, narrow, visible exception rather than
// a looser rule.
//
// Three restrictions keep an allowance from growing into a hole, and each is
// enforced by Scan rather than left to review:
//
//   - An entry names one exact repo-relative path. Never a directory, prefix,
//     pattern, or extension - those are what an allowance would quietly become.
//   - An entry must sit under a testdata directory, so no allowance can ever
//     cover source that ships inside the agent.
//   - An entry must still be needed. A path that is absent, outside Roots,
//     no longer binary, or carries no stated reason is a failure of its own,
//     rather than a permission that outlives the thing it was granted for.
var Allowed = map[string]string{}

// Violation is one file that breaks the rule, or one allowance that no longer
// earns its place.
type Violation struct {
	Path   string // repo-relative, slash-separated
	Reason string
}

func (v Violation) String() string { return v.Path + ": " + v.Reason }

// Scan reports every violation under Roots, sorted by path. A missing root is
// an error rather than an empty result: a renamed or mistyped root would
// otherwise read as a tree with nothing in it to reject.
func Scan(repoRoot string) ([]Violation, error) {
	var violations []Violation
	for _, root := range Roots {
		absolute := filepath.Join(repoRoot, filepath.FromSlash(root))
		if _, err := os.Stat(absolute); err != nil {
			return nil, fmt.Errorf("agent source root %s: %w", root, err)
		}
		err := filepath.WalkDir(absolute, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if entry.Name() == ".git" {
					return filepath.SkipDir
				}
				return nil
			}
			// .DS_Store is the one file macOS drops into any directory it
			// displays. The repository's .gitignore already names it, so it
			// cannot be committed, and failing a developer's check run over it
			// would teach people to work around this rule rather than keep it.
			if entry.Name() == ".DS_Store" {
				return nil
			}
			relative, err := filepath.Rel(repoRoot, path)
			if err != nil {
				return err
			}
			relative = filepath.ToSlash(relative)
			offset, culprit, binary, err := firstNonTextByte(path)
			if err != nil {
				// Fail closed. A file under the agent tree that cannot be read
				// is not a file anyone reviewed either.
				violations = append(violations, Violation{Path: relative, Reason: fmt.Sprintf("unreadable, so its content cannot be vouched for: %v", err)})
				return nil
			}
			if !binary {
				return nil
			}
			if _, allowed := Allowed[relative]; allowed {
				return nil
			}
			violations = append(violations, Violation{
				Path:   relative,
				Reason: fmt.Sprintf("binary content (byte %#02x at offset %d), which no file under the agent source tree may have", culprit, offset),
			})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walking agent source root %s: %w", root, err)
		}
	}
	violations = append(violations, staleAllowances(repoRoot)...)
	sort.Slice(violations, func(i, j int) bool {
		if violations[i].Path == violations[j].Path {
			return violations[i].Reason < violations[j].Reason
		}
		return violations[i].Path < violations[j].Path
	})
	return violations, nil
}

// staleAllowances reports every entry in Allowed that no longer earns its
// place, so an allowance cannot outlive the fixture it was granted for.
func staleAllowances(repoRoot string) []Violation {
	var violations []Violation
	for _, path := range sortedKeys(Allowed) {
		switch {
		case strings.TrimSpace(Allowed[path]) == "":
			violations = append(violations, Violation{Path: path, Reason: "allowance states no reason, so nobody can tell whether it is still needed"})
			continue
		case !underTestdata(path):
			violations = append(violations, Violation{Path: path, Reason: "allowance is available to test fixtures under a testdata directory only, never to source that ships inside the agent"})
			continue
		case !underRoots(path):
			violations = append(violations, Violation{Path: path, Reason: "allowance names a path outside the agent source tree, where this rule grants nothing"})
			continue
		}
		_, _, binary, err := firstNonTextByte(filepath.Join(repoRoot, filepath.FromSlash(path)))
		switch {
		case err != nil:
			violations = append(violations, Violation{Path: path, Reason: fmt.Sprintf("allowance names a file that cannot be read: %v", err)})
		case !binary:
			violations = append(violations, Violation{Path: path, Reason: "allowance is no longer needed: the file holds no binary content, so remove the allowance"})
		}
	}
	return violations
}

// underTestdata reports whether one of path's directories is named testdata.
// The file's own name does not count: a file called testdata is a file.
func underTestdata(path string) bool {
	parts := strings.Split(path, "/")
	for index := 0; index+1 < len(parts); index++ {
		if parts[index] == "testdata" {
			return true
		}
	}
	return false
}

// underRoots reports whether path sits inside one of Roots.
func underRoots(path string) bool {
	for _, root := range Roots {
		if strings.HasPrefix(path, root+"/") {
			return true
		}
	}
	return false
}

// firstNonTextByte reports the first byte of the file at path that text does
// not contain, if there is one.
//
// Detection is content-based on purpose. A list of binary extensions is a list
// somebody has to remember to extend, and it says nothing about a gzipped
// agent committed as "agent-data" or "notes.txt". Text here means valid UTF-8
// carrying no C0 control byte other than tab, newline, vertical tab, form feed
// and carriage return, and no DEL. That rejects every executable, archive and
// image format, accepts source in any language, and names the offending byte
// and its offset so the failure is actionable without a hex dump.
//
// The file is read as a stream rather than into memory, so the size of what
// somebody committed does not decide whether the rule can look at all of it.
func firstNonTextByte(path string) (offset int, culprit byte, binary bool, err error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0, false, err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	for {
		character, size, err := reader.ReadRune()
		if err == io.EOF {
			return 0, 0, false, nil
		}
		if err != nil {
			return 0, 0, false, err
		}
		if character == utf8.RuneError && size == 1 {
			if err := reader.UnreadRune(); err != nil {
				return 0, 0, false, err
			}
			invalid, err := reader.ReadByte()
			if err != nil {
				return 0, 0, false, err
			}
			return offset, invalid, true, nil
		}
		if (character < 0x20 && character != '\t' && character != '\n' && character != '\v' && character != '\f' && character != '\r') || character == 0x7f {
			return offset, byte(character), true, nil
		}
		offset += size
	}
}

func sortedKeys(entries map[string]string) []string {
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
