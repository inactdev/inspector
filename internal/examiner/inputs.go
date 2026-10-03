package examiner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maxInputBytes = 1 << 20

// Inputs names the three files the examiner is allowed to receive. The paths
// are copied into a private directory before the examiner container starts, so
// it never receives a parent directory that could also contain app source.
type Inputs struct {
	RequestPath     string
	GuidebookPath   string
	TestChangesPath string
}

// PreparedInputs is a private, disposable copy of the examiner's inputs.
type PreparedInputs struct {
	Dir         string
	TestChanges TestChangeList
}

// PrepareInputs reads and validates all three inputs, then copies only their
// bytes to a new directory. Call Cleanup after the examiner exits.
func PrepareInputs(inputs Inputs) (PreparedInputs, error) {
	request, err := readInput(inputs.RequestPath, "request")
	if err != nil {
		return PreparedInputs{}, err
	}
	if strings.TrimSpace(string(request)) == "" {
		return PreparedInputs{}, fmt.Errorf("reading request: file is empty")
	}
	guidebook, err := readInput(inputs.GuidebookPath, "guidebook")
	if err != nil {
		return PreparedInputs{}, err
	}
	if strings.TrimSpace(string(guidebook)) == "" {
		return PreparedInputs{}, fmt.Errorf("reading guidebook: file is empty")
	}
	testChanges, err := readInput(inputs.TestChangesPath, "test-change list")
	if err != nil {
		return PreparedInputs{}, err
	}
	list, err := ParseTestChanges(testChanges)
	if err != nil {
		return PreparedInputs{}, err
	}
	canonicalTestChanges, err := json.Marshal(list)
	if err != nil {
		return PreparedInputs{}, fmt.Errorf("serializing validated test-change list: %w", err)
	}

	dir, err := os.MkdirTemp("", "inspector-examiner-")
	if err != nil {
		return PreparedInputs{}, fmt.Errorf("creating examiner input directory: %w", err)
	}
	prepared := PreparedInputs{Dir: dir, TestChanges: list}
	for name, data := range map[string][]byte{
		InputRequestName: request, InputGuidebookName: guidebook, InputTestChangesName: canonicalTestChanges,
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

func readInput(path, name string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("reading %s: no path was provided", name)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s %q: %w", name, path, err)
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxInputBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s %q: %w", name, path, err)
	}
	if len(data) > maxInputBytes {
		return nil, fmt.Errorf("reading %s %q: exceeds the %d byte examiner input limit", name, path, maxInputBytes)
	}
	return data, nil
}

// ParseTestChanges accepts only test files shaped like *_test.*. This keeps the
// deliberate test-diff exception from becoming a channel for app source.
func ParseTestChanges(data []byte) (TestChangeList, error) {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return TestChangeList{}, fmt.Errorf("parsing test-change list: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var list TestChangeList
	if err := decoder.Decode(&list); err != nil {
		return TestChangeList{}, fmt.Errorf("parsing test-change list: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return TestChangeList{}, fmt.Errorf("parsing test-change list: contains more than one JSON value")
		}
		return TestChangeList{}, fmt.Errorf("parsing test-change list: %w", err)
	}
	if strings.TrimSpace(list.BaseCommit) == "" {
		return TestChangeList{}, fmt.Errorf("parsing test-change list: baseCommit is required")
	}
	for n, change := range list.Files {
		if !isTestPath(change.Path) {
			return TestChangeList{}, fmt.Errorf("parsing test-change list: file %d path %q is not a *_test.* file", n+1, change.Path)
		}
		switch change.Change {
		case "added":
			if change.Before != "" || change.After == "" || change.PreviousPath != "" {
				return TestChangeList{}, fmt.Errorf("parsing test-change list: added test %q must have only after content", change.Path)
			}
		case "modified":
			if change.Before == "" || change.After == "" || change.PreviousPath != "" {
				return TestChangeList{}, fmt.Errorf("parsing test-change list: modified test %q must have before and after content", change.Path)
			}
		case "deleted":
			if change.Before == "" || change.After != "" || change.PreviousPath != "" {
				return TestChangeList{}, fmt.Errorf("parsing test-change list: deleted test %q must have only before content", change.Path)
			}
		case "renamed":
			if !isTestPath(change.PreviousPath) || change.Before == "" || change.After == "" {
				return TestChangeList{}, fmt.Errorf("parsing test-change list: renamed test %q must name a *_test.* previousPath and have before and after content", change.Path)
			}
		default:
			return TestChangeList{}, fmt.Errorf("parsing test-change list: test %q has unknown change %q", change.Path, change.Change)
		}
	}
	return list, nil
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

func isTestPath(path string) bool {
	if path == "" || filepath.IsAbs(path) || strings.Contains(path, "\\") {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != path {
		return false
	}
	base := filepath.Base(clean)
	ext := filepath.Ext(base)
	return ext != "" && strings.HasSuffix(strings.TrimSuffix(base, ext), "_test")
}
