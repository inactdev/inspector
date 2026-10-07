package examiner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maxInputBytes = 1 << 20

// Inputs names the examiner's four documents plus the pre-task test versions
// Fabrica supplies for changed tests. All worker output is names-only.
type Inputs struct {
	RequestPath      string
	FeatureMapPath   string
	AlwaysTruePath   string
	ChangedFilesPath string
	BaseTestsPath    string
}

// PreparedInputs is a private, disposable copy of the examiner's validated
// inputs. The worker's bytes are never copied into the container.
type PreparedInputs struct {
	Dir          string
	ChangedFiles ChangedFileList
	BaseTests    BaseTestList
}

// PrepareInputs reads, validates, and copies the permitted inputs into a new
// directory. Structured input is re-serialized from validated data before the
// container starts. Call Cleanup after the examiner exits.
func PrepareInputs(inputs Inputs) (PreparedInputs, error) {
	return PrepareInputsContext(context.Background(), inputs)
}

// PrepareInputsContext respects ctx while preparing the sealed inputs.
func PrepareInputsContext(ctx context.Context, inputs Inputs) (PreparedInputs, error) {
	request, err := readInput(ctx, inputs.RequestPath, "request")
	if err != nil {
		return PreparedInputs{}, err
	}
	if strings.TrimSpace(string(request)) == "" {
		return PreparedInputs{}, errors.New("reading request: file is empty")
	}
	featureMap, err := readInput(ctx, inputs.FeatureMapPath, "feature map")
	if err != nil {
		return PreparedInputs{}, err
	}
	if strings.TrimSpace(string(featureMap)) == "" {
		return PreparedInputs{}, errors.New("reading feature map: file is empty")
	}
	alwaysTrue, err := readInput(ctx, inputs.AlwaysTruePath, "always-true list")
	if err != nil {
		return PreparedInputs{}, err
	}
	if strings.TrimSpace(string(alwaysTrue)) == "" {
		return PreparedInputs{}, errors.New("reading always-true list: file is empty")
	}
	changedFilesData, err := readInput(ctx, inputs.ChangedFilesPath, "changed-file list")
	if err != nil {
		return PreparedInputs{}, err
	}
	changedFiles, err := ParseChangedFiles(changedFilesData)
	if err != nil {
		return PreparedInputs{}, err
	}
	baseTestsData, err := readInput(ctx, inputs.BaseTestsPath, "base-test list")
	if err != nil {
		return PreparedInputs{}, err
	}
	baseTests, err := ParseBaseTests(baseTestsData, changedFiles)
	if err != nil {
		return PreparedInputs{}, err
	}
	if baseTests.BaseCommit != changedFiles.BaseCommit {
		return PreparedInputs{}, fmt.Errorf("parsing base-test list: baseCommit %q does not match changed-file list baseCommit %q", baseTests.BaseCommit, changedFiles.BaseCommit)
	}
	canonicalChangedFiles, err := json.Marshal(changedFiles)
	if err != nil {
		return PreparedInputs{}, fmt.Errorf("serializing validated changed-file list: %w", err)
	}
	canonicalBaseTests, err := json.Marshal(baseTests)
	if err != nil {
		return PreparedInputs{}, fmt.Errorf("serializing validated base-test list: %w", err)
	}

	dir, err := os.MkdirTemp("", "inspector-examiner-")
	if err != nil {
		return PreparedInputs{}, fmt.Errorf("creating examiner input directory: %w", err)
	}
	prepared := PreparedInputs{Dir: dir, ChangedFiles: changedFiles, BaseTests: baseTests}
	for name, data := range map[string][]byte{
		InputRequestName: request, InputFeatureMapName: featureMap, InputAlwaysTrueName: alwaysTrue,
		InputChangedFilesName: canonicalChangedFiles, InputBaseTestsName: canonicalBaseTests,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o400); err != nil {
			prepared.Cleanup()
			return PreparedInputs{}, fmt.Errorf("copying %s for the examiner: %w", name, err)
		}
	}
	return prepared, nil
}

// Cleanup removes the disposable input directory.
func (p PreparedInputs) Cleanup() {
	if p.Dir != "" {
		_ = os.RemoveAll(p.Dir)
	}
}

func readInput(ctx context.Context, path, name string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("reading %s: no path was provided", name)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("reading %s %q: %w", name, path, err)
	}
	file, err := openRegularInput(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s %q: %w", name, path, err)
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxInputBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s %q: %w", name, path, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("reading %s %q: %w", name, path, err)
	}
	if len(data) > maxInputBytes {
		return nil, fmt.Errorf("reading %s %q: exceeds the %d byte examiner input limit", name, path, maxInputBytes)
	}
	return data, nil
}

// ParseChangedFiles accepts every worker-changed path by name only. The list
// must explicitly contain files, even when it is empty, so an unreadable
// producer output can never be mistaken for no changes.
func ParseChangedFiles(data []byte) (ChangedFileList, error) {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return ChangedFileList{}, fmt.Errorf("parsing changed-file list: %w", err)
	}
	if err := requireJSONArrayField(data, "files"); err != nil {
		return ChangedFileList{}, fmt.Errorf("parsing changed-file list: %w", err)
	}
	var list ChangedFileList
	if err := decodeStrictJSON(data, &list); err != nil {
		return ChangedFileList{}, fmt.Errorf("parsing changed-file list: %w", err)
	}
	if strings.TrimSpace(list.BaseCommit) == "" {
		return ChangedFileList{}, errors.New("parsing changed-file list: baseCommit is required")
	}
	seen := map[string]struct{}{}
	for n, file := range list.Files {
		if !isRelativePath(file.Path) {
			return ChangedFileList{}, fmt.Errorf("parsing changed-file list: file %d path %q is not a clean relative path", n+1, file.Path)
		}
		if _, exists := seen[file.Path]; exists {
			return ChangedFileList{}, fmt.Errorf("parsing changed-file list: file path %q appears more than once", file.Path)
		}
		seen[file.Path] = struct{}{}
		switch file.Change {
		case "added", "modified", "deleted":
			if file.PreviousPath != "" {
				return ChangedFileList{}, fmt.Errorf("parsing changed-file list: %s file %q may not have previousPath", file.Change, file.Path)
			}
		case "renamed":
			if !isRelativePath(file.PreviousPath) {
				return ChangedFileList{}, fmt.Errorf("parsing changed-file list: renamed file %q needs a clean previousPath", file.Path)
			}
		default:
			return ChangedFileList{}, fmt.Errorf("parsing changed-file list: file %q has unknown change %q", file.Path, file.Change)
		}
	}
	return list, nil
}

// ParseBaseTests accepts only task-starting versions of test files that the
// changed-file list already named. New tests have no entry and trigger nothing.
func ParseBaseTests(data []byte, changed ChangedFileList) (BaseTestList, error) {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return BaseTestList{}, fmt.Errorf("parsing base-test list: %w", err)
	}
	if err := requireJSONArrayField(data, "tests"); err != nil {
		return BaseTestList{}, fmt.Errorf("parsing base-test list: %w", err)
	}
	var list BaseTestList
	if err := decodeStrictJSON(data, &list); err != nil {
		return BaseTestList{}, fmt.Errorf("parsing base-test list: %w", err)
	}
	if strings.TrimSpace(list.BaseCommit) == "" {
		return BaseTestList{}, errors.New("parsing base-test list: baseCommit is required")
	}
	changedTests := changedTestPaths(changed)
	seen := map[string]struct{}{}
	for n, test := range list.Tests {
		if !isTestPath(test.Path) {
			return BaseTestList{}, fmt.Errorf("parsing base-test list: test %d path %q is not a *_test.* file", n+1, test.Path)
		}
		if _, exists := changedTests[test.Path]; !exists {
			return BaseTestList{}, fmt.Errorf("parsing base-test list: %q was not a changed test file", test.Path)
		}
		if _, exists := seen[test.Path]; exists {
			return BaseTestList{}, fmt.Errorf("parsing base-test list: %q appears more than once", test.Path)
		}
		seen[test.Path] = struct{}{}
	}
	return list, nil
}

func changedTestPaths(changed ChangedFileList) map[string]struct{} {
	paths := make(map[string]struct{})
	for _, file := range changed.Files {
		if isTestPath(file.Path) {
			paths[file.Path] = struct{}{}
		}
		if isTestPath(file.PreviousPath) {
			paths[file.PreviousPath] = struct{}{}
		}
	}
	return paths
}

func requireJSONArrayField(data []byte, field string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	value, ok := fields[field]
	if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return fmt.Errorf("%s is required and must be an array", field)
	}
	var array []json.RawMessage
	if err := json.Unmarshal(value, &array); err != nil {
		return fmt.Errorf("%s must be an array", field)
	}
	return nil
}

func decodeStrictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("contains more than one JSON value")
		}
		return err
	}
	return nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var readValue func() error
	readValue = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			keys := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("object key is not a string")
				}
				if _, exists := keys[key]; exists {
					return fmt.Errorf("duplicate field %q", key)
				}
				keys[key] = struct{}{}
				if err := readValue(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := readValue(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return fmt.Errorf("unexpected delimiter %q", delim)
		}
	}
	if err := readValue(); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return errors.New("contains more than one JSON value")
		}
		return err
	}
	return nil
}

func isRelativePath(path string) bool {
	if path == "" || filepath.IsAbs(path) || strings.Contains(path, "\\") {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	return clean != "." && clean != ".." && !strings.HasPrefix(clean, "../") && clean == path
}

func isTestPath(path string) bool {
	if !isRelativePath(path) {
		return false
	}
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	return ext != "" && strings.HasSuffix(strings.TrimSuffix(base, ext), "_test")
}
