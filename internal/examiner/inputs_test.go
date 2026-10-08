package examiner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareInputs_CopiesOnlyThePermittedInputs(t *testing.T) {
	dir := t.TempDir()
	paths := writeInputFixture(t, dir)
	prepared, err := PrepareInputs(Inputs{
		RequestPath: paths[InputRequestName], FeatureMapPath: paths[InputFeatureMapName], AlwaysTruePath: paths[InputAlwaysTrueName],
		ChangedFilesPath: paths[InputChangedFilesName], BaseTestsPath: paths[InputBaseTestsName],
	})
	if err != nil {
		t.Fatalf("PrepareInputs() error = %v", err)
	}
	defer prepared.Cleanup()
	entries, err := os.ReadDir(prepared.Dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 5 {
		t.Fatalf("input directory has %d entries, want exactly the permitted inputs", len(entries))
	}
	want := map[string]bool{InputRequestName: true, InputFeatureMapName: true, InputAlwaysTrueName: true, InputChangedFilesName: true, InputBaseTestsName: true}
	for _, entry := range entries {
		if !want[entry.Name()] {
			t.Fatalf("input directory unexpectedly contains %q", entry.Name())
		}
		if entry.Type()&os.ModeSymlink != 0 {
			t.Fatalf("input %q is a symlink instead of a copied file", entry.Name())
		}
	}
}

func TestParseChangedFiles_AllowsSourceNamesButNoSourceContent(t *testing.T) {
	list, err := ParseChangedFiles([]byte(`{
		"baseCommit":"abc123",
		"files":[{"path":"backend/server.go","change":"modified"}]
	}`))
	if err != nil {
		t.Fatalf("ParseChangedFiles() error = %v", err)
	}
	if list.Files[0].Path != "backend/server.go" {
		t.Fatalf("list = %#v, want source filename only", list)
	}
	_, err = ParseChangedFiles([]byte(`{
		"baseCommit":"abc123",
		"files":[{"path":"backend/server.go","change":"modified","after":"source is forbidden"}]
	}`))
	if err == nil {
		t.Fatal("ParseChangedFiles() accepted worker content")
	}
}

func TestParseChangedFiles_RequiresExplicitFilesArray(t *testing.T) {
	for name, input := range map[string]string{
		"missing": `{"baseCommit":"abc123"}`,
		"null":    `{"baseCommit":"abc123","files":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseChangedFiles([]byte(input)); err == nil {
				t.Fatal("ParseChangedFiles() accepted a missing changed-file list")
			}
		})
	}
	if _, err := ParseChangedFiles([]byte(`{"baseCommit":"abc123","files":[]}`)); err != nil {
		t.Fatalf("ParseChangedFiles() rejected an explicitly empty changed-file list: %v", err)
	}
}

func TestParseBaseTests_AcceptsOnlyChangedPreTaskTests(t *testing.T) {
	changed, err := ParseChangedFiles([]byte(`{"baseCommit":"abc123","files":[{"path":"capture_test.go","change":"modified"}]}`))
	if err != nil {
		t.Fatalf("ParseChangedFiles() error = %v", err)
	}
	base, err := ParseBaseTests([]byte(`{"baseCommit":"abc123","tests":[{"path":"capture_test.go","content":"func TestCapture(t *testing.T) {}"}]}`), changed)
	if err != nil {
		t.Fatalf("ParseBaseTests() error = %v", err)
	}
	if len(base.Tests) != 1 {
		t.Fatalf("base tests = %#v, want one test", base)
	}
	_, err = ParseBaseTests([]byte(`{"baseCommit":"abc123","tests":[{"path":"server_test.go","content":"not changed"}]}`), changed)
	if err == nil {
		t.Fatal("ParseBaseTests() accepted a test not named by changed files")
	}
}

func TestParseBaseTests_RequiresExactlyTaskStartingChangedTests(t *testing.T) {
	for _, test := range []struct {
		name        string
		changed     string
		baseTests   string
		wantFailure bool
	}{
		{
			name: "modified test is required", changed: `[{"path":"capture_test.go","change":"modified"}]`,
			baseTests: `[]`, wantFailure: true,
		},
		{
			name: "added test has no base version", changed: `[{"path":"capture_test.go","change":"added"}]`,
			baseTests: `[{"path":"capture_test.go","content":"fabricated"}]`, wantFailure: true,
		},
		{
			name: "deleted test retains base version", changed: `[{"path":"capture_test.go","change":"deleted"}]`,
			baseTests: `[{"path":"capture_test.go","content":"old test"}]`,
		},
		{
			name: "renamed test uses previous path", changed: `[{"path":"renamed_test.go","previousPath":"capture_test.go","change":"renamed"}]`,
			baseTests: `[{"path":"capture_test.go","content":"old test"}]`,
		},
		{
			name: "renamed test rejects current path", changed: `[{"path":"renamed_test.go","previousPath":"capture_test.go","change":"renamed"}]`,
			baseTests: `[{"path":"renamed_test.go","content":"worker test"}]`, wantFailure: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed, err := ParseChangedFiles([]byte(`{"baseCommit":"abc123","files":` + test.changed + `}`))
			if err != nil {
				t.Fatalf("ParseChangedFiles() error = %v", err)
			}
			_, err = ParseBaseTests([]byte(`{"baseCommit":"abc123","tests":`+test.baseTests+`}`), changed)
			if (err != nil) != test.wantFailure {
				t.Fatalf("ParseBaseTests() error = %v, wantFailure %v", err, test.wantFailure)
			}
		})
	}
}

func TestPrepareInputs_CanonicalizesWorkerLists(t *testing.T) {
	dir := t.TempDir()
	paths := writeInputFixture(t, dir)
	if err := os.WriteFile(paths[InputChangedFilesName], []byte("{\n  \"files\": [],\n  \"baseCommit\": \"abc123\"\n}\n"), 0o600); err != nil {
		t.Fatalf("writing changed files: %v", err)
	}
	prepared, err := PrepareInputs(Inputs{
		RequestPath: paths[InputRequestName], FeatureMapPath: paths[InputFeatureMapName], AlwaysTruePath: paths[InputAlwaysTrueName],
		ChangedFilesPath: paths[InputChangedFilesName], BaseTestsPath: paths[InputBaseTestsName],
	})
	if err != nil {
		t.Fatalf("PrepareInputs() error = %v", err)
	}
	defer prepared.Cleanup()
	data, err := os.ReadFile(filepath.Join(prepared.Dir, InputChangedFilesName))
	if err != nil {
		t.Fatalf("reading prepared changed files: %v", err)
	}
	want, err := json.Marshal(prepared.ChangedFiles)
	if err != nil {
		t.Fatalf("marshaling parsed changed files: %v", err)
	}
	if string(data) != string(want) {
		t.Fatalf("prepared changed files = %q, want canonical validated JSON %q", data, want)
	}
}

func writeInputFixture(t *testing.T, dir string) map[string]string {
	t.Helper()
	paths := map[string]string{}
	for name, content := range map[string]string{
		InputRequestName:      "Add capture.",
		InputFeatureMapName:   "POST /inklings creates a capture.",
		InputAlwaysTrueName:   "Existing captures remain readable.",
		InputChangedFilesName: `{"baseCommit":"abc123","files":[]}`,
		InputBaseTestsName:    `{"baseCommit":"abc123","tests":[]}`,
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
		paths[name] = path
	}
	return paths
}
