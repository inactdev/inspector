package runtimeidentity

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed go.mod runtime_identity.go cmd/examiner-agent/*.go internal/container/*.go internal/examiner/*.go internal/examiner/runtime/Dockerfile
var runtimeInputs embed.FS

func Fingerprint() string {
	var names []string
	err := fs.WalkDir(runtimeInputs, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && !strings.HasSuffix(path, "_test.go") {
			names = append(names, path)
		}
		return nil
	})
	if err != nil {
		panic(fmt.Sprintf("reading embedded examiner runtime inputs: %v", err))
	}
	sort.Strings(names)
	hash := sha256.New()
	for _, name := range names {
		data, err := runtimeInputs.ReadFile(name)
		if err != nil {
			panic(fmt.Sprintf("reading embedded examiner runtime input %s: %v", name, err))
		}
		_, _ = fmt.Fprintf(hash, "%d:%s:%d:", len(name), name, len(data))
		_, _ = hash.Write(data)
	}
	return hex.EncodeToString(hash.Sum(nil))
}
