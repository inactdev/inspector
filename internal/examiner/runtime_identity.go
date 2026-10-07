package examiner

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"sort"
)

//go:embed *.go
var runtimeSources embed.FS

func RuntimeSourceFingerprint() string {
	entries, err := runtimeSources.ReadDir(".")
	if err != nil {
		panic(fmt.Sprintf("reading embedded examiner sources: %v", err))
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	hash := sha256.New()
	for _, name := range names {
		data, err := runtimeSources.ReadFile(name)
		if err != nil {
			panic(fmt.Sprintf("reading embedded examiner source %s: %v", name, err))
		}
		_, _ = fmt.Fprintf(hash, "%d:%s:%d:", len(name), name, len(data))
		_, _ = hash.Write(data)
	}
	return hex.EncodeToString(hash.Sum(nil))
}
