package agentsource

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAgentSourceTreeHasNoBinaryFiles is the rule itself, run against this
// repository on every check. It is deliberately not the whole of issue #36:
// a test that only asserts a clean tree passes would also pass if Scan looked
// at nothing at all, which is what the tests below exist to rule out.
func TestAgentSourceTreeHasNoBinaryFiles(t *testing.T) {
	root := repoRoot(t)
	violations, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(violations) > 0 {
		t.Fatalf("Inspector's agent source tree carries content no reviewer can read:\n%s\n\n"+
			"No file under %s may contain binary content. Build the agent from source "+
			"instead - internal/examiner/runtime/Dockerfile already does. A test fixture "+
			"that genuinely needs binary bytes goes under a testdata directory and into "+
			"agentsource.Allowed with its reason.", format(violations), strings.Join(Roots, ", "))
	}
}

// TestScanRejectsABinaryFileAddedToTheRealAgentTree attempts the forbidden
// thing: it puts a gzipped "agent binary" into the real agent source tree,
// exactly where PR #33's first runtime put two of them, and requires the
// shipped rule to reject it. Then it takes it away again and requires the tree
// to read clean, so this test cannot be what leaves a binary behind.
func TestScanRejectsABinaryFileAddedToTheRealAgentTree(t *testing.T) {
	root := repoRoot(t)
	relative := "internal/examiner/runtime/negative-test-examiner-agent.gz"
	path := filepath.Join(root, filepath.FromSlash(relative))

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			t.Fatalf("%s already exists; an interrupted earlier run left it behind - delete it", relative)
		}
		// Issue #27 will make a checked-out source tree read-only while a
		// check runs. Skip rather than fail there: the rule still has to be
		// provable, and TestScanRejectsBinaryContentUnderEveryGuardedRoot
		// proves it against every root without writing to this repository.
		t.Skipf("cannot write %s (%v); the agent tree is not writable here", relative, err)
	}
	t.Cleanup(func() { os.Remove(path) })
	if _, err := file.Write(gzipped(t, "a Linux agent binary no reviewer can read")); err != nil {
		file.Close()
		t.Fatalf("writing %s: %v", relative, err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("closing %s: %v", relative, err)
	}

	violations, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if !reports(violations, relative) {
		t.Fatalf("Scan() accepted a gzipped binary at %s; violations = %s", relative, format(violations))
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("removing %s: %v", relative, err)
	}
	violations, err = Scan(root)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(violations) > 0 {
		t.Fatalf("tree not clean after removing %s: %s", relative, format(violations))
	}
}

// TestScanRejectsBinaryContentUnderEveryGuardedRoot proves the rule covers
// each named root rather than only the directory the binaries happened to be
// in, and that it reads bytes rather than file names: the forbidden file is
// called notes.txt in every case.
func TestScanRejectsBinaryContentUnderEveryGuardedRoot(t *testing.T) {
	root := mirrorRoots(t)
	for _, guarded := range Roots {
		write(t, root, guarded+"/notes.txt", gzipped(t, "an agent binary with a text file's name"))
		write(t, root, guarded+"/README.md", []byte("# text is fine\n\ttabs, CRLF\r\naccents: é, emoji: 🚦\n"))
	}
	violations, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(violations) != len(Roots) {
		t.Fatalf("Scan() reported %s, want exactly one violation per root", format(violations))
	}
	for _, guarded := range Roots {
		if !reports(violations, guarded+"/notes.txt") {
			t.Fatalf("Scan() accepted binary content under %s; violations = %s", guarded, format(violations))
		}
	}
}

func TestScanRequiresEveryRootToBeThere(t *testing.T) {
	root := mirrorRoots(t)
	for _, guarded := range Roots {
		if err := os.RemoveAll(filepath.Join(root, filepath.FromSlash(guarded))); err != nil {
			t.Fatalf("RemoveAll() error = %v", err)
		}
		if _, err := Scan(root); err == nil {
			t.Fatalf("Scan() accepted a tree with no %s; a renamed root must not read as nothing to reject", guarded)
		}
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(guarded)), 0o755); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
	}
}

func TestScanDetectsBinaryContentByItsBytes(t *testing.T) {
	binary := map[string][]byte{
		"gzip":                     gzipped(t, "an agent binary"),
		"elf header":               {0x7f, 'E', 'L', 'F', 0x02, 0x01, 0x01, 0x00},
		"png header":               {0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a},
		"utf-16 text":              {0x68, 0x00, 0x69, 0x00},
		"text with a trailing nul": append([]byte("looks like source\n"), 0x00),
		"lone escape byte":         []byte("menu\x1b[31m"),
		"invalid utf-8":            {'a', 0xc3, 0x28},
	}
	for name, content := range binary {
		if _, _, isBinary, err := firstNonTextByte(writeTemp(t, content)); err != nil || !isBinary {
			t.Fatalf("%s read as text (err = %v)", name, err)
		}
	}
	text := map[string][]byte{
		"empty file":          {},
		"go source":           []byte("package main\n\nfunc main() {}\n"),
		"crlf and tabs":       []byte("one\r\n\ttwo\n\vthree\ffour\n"),
		"utf-8 beyond ascii":  []byte("héllo, 🚦, 中文\n"),
		"no trailing newline": []byte("last line"),
	}
	for name, content := range text {
		if _, _, isBinary, err := firstNonTextByte(writeTemp(t, content)); err != nil || isBinary {
			t.Fatalf("%s read as binary (err = %v)", name, err)
		}
	}
}

func TestAllowanceCarriesAStatedTestdataFixture(t *testing.T) {
	root := mirrorRoots(t)
	fixture := "internal/examiner/testdata/sealed-input.gz"
	write(t, root, fixture, gzipped(t, "a fixture that genuinely needs binary bytes"))
	withAllowed(t, map[string]string{fixture: "a gzipped sealed input the examiner must refuse to read"})
	violations, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(violations) > 0 {
		t.Fatalf("Scan() rejected an allowed testdata fixture: %s", format(violations))
	}
}

func TestAllowanceIsRefusedWhenItStopsEarningItsPlace(t *testing.T) {
	binaryFixture := "internal/examiner/testdata/sealed-input.gz"
	textFixture := "internal/examiner/testdata/sealed-input.json"
	shippedSource := "internal/examiner/runtime/examiner-agent.gz"
	outsideTree := "cmd/inspector/testdata/report.gz"
	cases := map[string]map[string]string{
		"no reason given":      {binaryFixture: "   "},
		"not under testdata":   {shippedSource: "the agent binary, which is exactly what this rule forbids"},
		"outside the roots":    {outsideTree: "a fixture this rule grants nothing for"},
		"nothing at that path": {"internal/examiner/testdata/gone.gz": "a fixture that was deleted"},
		"no longer binary":     {textFixture: "a fixture that has since become JSON"},
	}
	for name, allowed := range cases {
		t.Run(name, func(t *testing.T) {
			root := mirrorRoots(t)
			write(t, root, binaryFixture, gzipped(t, "a fixture"))
			write(t, root, textFixture, []byte("{\"sealed\": true}\n"))
			write(t, root, shippedSource, gzipped(t, "an agent binary"))
			write(t, root, outsideTree, gzipped(t, "a report"))
			withAllowed(t, allowed)
			violations, err := Scan(root)
			if err != nil {
				t.Fatalf("Scan() error = %v", err)
			}
			for path := range allowed {
				if !reports(violations, path) {
					t.Fatalf("Scan() honoured the allowance for %s; violations = %s", path, format(violations))
				}
			}
		})
	}
}

// repoRoot is this package's directory, two levels down from the repository it
// is a rule about.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repository root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("%s is not the repository root: %v", root, err)
	}
	return root
}

// mirrorRoots builds an empty copy of the real tree's shape, so a test can add
// the forbidden thing without touching this repository.
func mirrorRoots(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, guarded := range Roots {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(guarded)), 0o755); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
	}
	return root
}

func write(t *testing.T, root, relative string, content []byte) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", relative, err)
	}
}

func writeTemp(t *testing.T, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sample")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

func withAllowed(t *testing.T, entries map[string]string) {
	t.Helper()
	shipped := Allowed
	Allowed = entries
	t.Cleanup(func() { Allowed = shipped })
}

func gzipped(t *testing.T, content string) []byte {
	t.Helper()
	var out bytes.Buffer
	writer := gzip.NewWriter(&out)
	if _, err := writer.Write([]byte(content)); err != nil {
		t.Fatalf("gzip write error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("gzip close error = %v", err)
	}
	return out.Bytes()
}

func reports(violations []Violation, path string) bool {
	for _, violation := range violations {
		if violation.Path == path {
			return true
		}
	}
	return false
}

func format(violations []Violation) string {
	lines := make([]string, 0, len(violations))
	for _, violation := range violations {
		lines = append(lines, "  "+violation.String())
	}
	return strings.Join(lines, "\n")
}
