package examiner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareInputs_CopiesOnlyTheThreeAllowedInputs(t *testing.T) {
	dir := t.TempDir()
	request := filepath.Join(dir, "request.md")
	guidebook := filepath.Join(dir, "guidebook.md")
	testChanges := filepath.Join(dir, "changes.json")
	for path, content := range map[string]string{
		request:     "Add capture.",
		guidebook:   "POST /inklings creates a capture.",
		testChanges: `{"baseCommit":"abc123","files":[]}`,
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}

	prepared, err := PrepareInputs(Inputs{RequestPath: request, GuidebookPath: guidebook, TestChangesPath: testChanges})
	if err != nil {
		t.Fatalf("PrepareInputs() error = %v", err)
	}
	defer prepared.Cleanup()
	entries, err := os.ReadDir(prepared.Dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("input directory has %d entries, want exactly the three permitted inputs", len(entries))
	}
	want := map[string]bool{InputRequestName: true, InputGuidebookName: true, InputTestChangesName: true}
	for _, entry := range entries {
		if !want[entry.Name()] {
			t.Fatalf("input directory unexpectedly contains %q", entry.Name())
		}
		if entry.Type()&os.ModeSymlink != 0 {
			t.Fatalf("input %q is a symlink instead of a copied file", entry.Name())
		}
	}
}

func TestParseTestChanges_RejectsApplicationSource(t *testing.T) {
	_, err := ParseTestChanges([]byte(`{
		"baseCommit":"abc123",
		"files":[{"path":"backend/server.go","change":"modified","before":"old","after":"new"}]
	}`))
	if err == nil {
		t.Fatal("ParseTestChanges() accepted application source in the test-change exception")
	}
}

func TestParseTestChanges_RequiresFilesArray(t *testing.T) {
	for name, input := range map[string]string{
		"missing": `{"baseCommit":"abc123"}`,
		"null":    `{"baseCommit":"abc123","files":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseTestChanges([]byte(input)); err == nil {
				t.Fatal("ParseTestChanges() accepted a missing test-change list")
			}
		})
	}
	if _, err := ParseTestChanges([]byte(`{"baseCommit":"abc123","files":[]}`)); err != nil {
		t.Fatalf("ParseTestChanges() rejected an explicitly empty test-change list: %v", err)
	}
}

func TestParseTestChanges_RejectsUnrecognizedChannels(t *testing.T) {
	_, err := ParseTestChanges([]byte(`{
		"baseCommit":"abc123",
		"files":[],
		"implementationDiff":"not allowed"
	}`))
	if err == nil {
		t.Fatal("ParseTestChanges() accepted an implementation-diff field")
	}
}

func TestParseTestChanges_RejectsDuplicateSourceChannel(t *testing.T) {
	_, err := ParseTestChanges([]byte(`{
		"baseCommit":"abc123",
		"files":[{"path":"backend/server.go","change":"modified","before":"old","after":"new"}],
		"files":[]
	}`))
	if err == nil {
		t.Fatal("ParseTestChanges() accepted a duplicate field hiding application source")
	}
}

func TestPrepareInputs_WritesCanonicalTestChanges(t *testing.T) {
	dir := t.TempDir()
	request := filepath.Join(dir, "request.md")
	guidebook := filepath.Join(dir, "guidebook.md")
	testChanges := filepath.Join(dir, "changes.json")
	for path, content := range map[string]string{
		request:     "Add capture.",
		guidebook:   "POST /inklings creates a capture.",
		testChanges: "{\n  \"files\": [],\n  \"baseCommit\": \"abc123\"\n}\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}

	prepared, err := PrepareInputs(Inputs{RequestPath: request, GuidebookPath: guidebook, TestChangesPath: testChanges})
	if err != nil {
		t.Fatalf("PrepareInputs() error = %v", err)
	}
	defer prepared.Cleanup()
	data, err := os.ReadFile(filepath.Join(prepared.Dir, InputTestChangesName))
	if err != nil {
		t.Fatalf("reading prepared test changes: %v", err)
	}
	want, err := json.Marshal(prepared.TestChanges)
	if err != nil {
		t.Fatalf("marshaling parsed test changes: %v", err)
	}
	if string(data) != string(want) {
		t.Fatalf("prepared test changes = %q, want canonical validated JSON %q", data, want)
	}
}
