//go:build !examiner_agent

package examiner

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	agentLinuxAMD64SHA256 = "d85c8340433c4914002a7a46b2efae724479b371abab558285ea401e572fc8e1"
	agentLinuxARM64SHA256 = "e3ed4748ef158b3f6ecd4796bef1f69df5ed7769dd0dc6dc7453b7440065e9d0"
)

//go:embed runtime/examiner-agent-linux-amd64.gz
var agentLinuxAMD64 []byte

//go:embed runtime/examiner-agent-linux-arm64.gz
var agentLinuxARM64 []byte

func prepareAgentExecutable() (string, func(), error) {
	target, err := dockerServerTarget()
	if err != nil {
		return "", func() {}, err
	}
	var compressed []byte
	var expectedHash string
	switch target {
	case "linux/amd64":
		compressed, expectedHash = agentLinuxAMD64, agentLinuxAMD64SHA256
	case "linux/arm64":
		compressed, expectedHash = agentLinuxARM64, agentLinuxARM64SHA256
	default:
		return "", func() {}, fmt.Errorf("Docker server platform %q has no bundled examiner agent", target)
	}
	actualHash := sha256.Sum256(compressed)
	if hex.EncodeToString(actualHash[:]) != expectedHash {
		return "", func() {}, fmt.Errorf("bundled examiner agent for %s failed its integrity check", target)
	}

	dir, err := os.MkdirTemp("", "inspector-examiner-agent-")
	if err != nil {
		return "", func() {}, fmt.Errorf("creating examiner agent directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("opening bundled examiner agent for %s: %w", target, err)
	}
	path := filepath.Join(dir, "examiner-agent")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o500)
	if err != nil {
		reader.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("creating examiner agent for %s: %w", target, err)
	}
	_, copyErr := io.Copy(file, reader)
	closeErr := file.Close()
	readerErr := reader.Close()
	if copyErr != nil || closeErr != nil || readerErr != nil {
		cleanup()
		for _, candidate := range []error{copyErr, closeErr, readerErr} {
			if candidate != nil {
				return "", func() {}, fmt.Errorf("extracting bundled examiner agent for %s: %w", target, candidate)
			}
		}
	}
	return path, cleanup, nil
}

func dockerServerTarget() (string, error) {
	output, err := exec.Command("docker", "version", "--format", "{{.Server.Os}}/{{.Server.Arch}}").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("reading Docker server platform: %w: %s", err, strings.TrimSpace(string(output)))
	}
	target := strings.TrimSpace(string(output))
	switch target {
	case "linux/x86_64":
		target = "linux/amd64"
	case "linux/aarch64":
		target = "linux/arm64"
	}
	return target, nil
}
