package inspector

import (
	"errors"
	"strings"
	"testing"

	"github.com/inactdev/inspector/internal/container"
)

func TestResolveBuildArgs_NoneIsNothing(t *testing.T) {
	// A project that declares none must come out of this exactly as it did
	// before the key existed: nothing passed to the build, nothing folded
	// into the tag, so nothing rebuilt.
	repo := newTestRepo(t, nil)
	commit := strings.TrimSpace(runGitT(t, repo, "rev-parse", "HEAD"))

	for _, sources := range []map[string]BuildArgSource{nil, {}} {
		args, err := ResolveBuildArgs(repo, commit, sources)
		if err != nil {
			t.Fatalf("resolving %v: %v", sources, err)
		}
		if args != nil {
			t.Fatalf("args = %v, want nil", args)
		}
	}
}

func TestResolveBuildArgs_ReadsTheFirstNonEmptyLineOfTheCommittedFile(t *testing.T) {
	repo := newTestRepo(t, map[string]string{
		// A leading blank line and trailing whitespace, which real
		// version files carry and no Dockerfile wants.
		".ruby-version": "\n  3.4.13  \n",
		".nvmrc":        "20.11.1\n",
	})
	commit := strings.TrimSpace(runGitT(t, repo, "rev-parse", "HEAD"))

	args, err := ResolveBuildArgs(repo, commit, map[string]BuildArgSource{
		"RUBY_VERSION": {File: ".ruby-version"},
		"NODE_VERSION": {File: ".nvmrc"},
	})
	if err != nil {
		t.Fatalf("resolving two declared build arguments: %v", err)
	}
	// Sorted by name, because that order is what the tag hashes and what
	// the docker command line carries - neither may depend on a Go map.
	if len(args) != 2 || args[0].Name != "NODE_VERSION" || args[1].Name != "RUBY_VERSION" {
		t.Fatalf("args = %+v, want both, sorted by name", args)
	}
	if args[0].Value != "20.11.1" || args[1].Value != "3.4.13" {
		t.Fatalf("args = %+v, want each file's first non-empty line, trimmed", args)
	}
}

// The value is a fact about the commit, not about the directory. Run
// refuses a dirty tree, so for a tracked file the two are the same bytes -
// but what is read is the commit's, and these are the two cases where that
// is the difference between an honest tag and a silent one.
func TestResolveBuildArgs_ReadsTheCommitRatherThanTheWorkingTree(t *testing.T) {
	repo := newTestRepo(t, map[string]string{
		".ruby-version": "3.4.13\n",
		".gitignore":    ".tool-version\n",
	})
	commit := strings.TrimSpace(runGitT(t, repo, "rev-parse", "HEAD"))

	// An uncommitted edit - which Run refuses separately, and which this
	// must not read even so.
	writeFiles(t, repo, map[string]string{".ruby-version": "9.9.9\n"})
	args, err := ResolveBuildArgs(repo, commit, map[string]BuildArgSource{"RUBY_VERSION": {File: ".ruby-version"}})
	if err != nil {
		t.Fatalf("resolving against the commit: %v", err)
	}
	if len(args) != 1 || args[0].Value != "3.4.13" {
		t.Fatalf("args = %+v, want the commit's 3.4.13 rather than the working tree's 9.9.9", args)
	}

	// And a gitignored file, which leaves the tree clean and is in no
	// commit at all - so it could otherwise have set the version the check
	// image was built with while the commit declared nothing of the sort.
	writeFiles(t, repo, map[string]string{".tool-version": "7.7.7\n"})
	_, err = ResolveBuildArgs(repo, commit, map[string]BuildArgSource{"TOOL_VERSION": {File: ".tool-version"}})
	if !errors.Is(err, container.ErrBuildArgUnavailable) {
		t.Fatalf("err = %v, want a refusal wrapping container.ErrBuildArgUnavailable for a file the commit does not hold", err)
	}
	if !strings.Contains(err.Error(), "TOOL_VERSION") || !strings.Contains(err.Error(), ".tool-version") {
		t.Fatalf("err = %q, want it to name the argument and the file", err)
	}
}

func TestResolveBuildArgs_PatternCapturesAValueInsideABiggerFile(t *testing.T) {
	repo := newTestRepo(t, map[string]string{
		"go.mod": "module example.com/x\n\ngo 1.22.5\n\nrequire github.com/x/y v1.2.3\n",
		// The version is on the line *after* its marker, which is why the
		// pattern is matched against the whole file rather than line by
		// line.
		"Gemfile.lock": "DEPENDENCIES\n  rails\n\nBUNDLED WITH\n   2.5.6\n",
	})
	commit := strings.TrimSpace(runGitT(t, repo, "rev-parse", "HEAD"))

	args, err := ResolveBuildArgs(repo, commit, map[string]BuildArgSource{
		"GO_VERSION":      {File: "go.mod", Pattern: `(?m)^go (\S+)$`},
		"BUNDLER_VERSION": {File: "Gemfile.lock", Pattern: `BUNDLED WITH\s+(\S+)`},
	})
	if err != nil {
		t.Fatalf("resolving two patterned build arguments: %v", err)
	}
	want := map[string]string{"BUNDLER_VERSION": "2.5.6", "GO_VERSION": "1.22.5"}
	for _, arg := range args {
		if want[arg.Name] != arg.Value {
			t.Fatalf("%s = %q, want %q", arg.Name, arg.Value, want[arg.Name])
		}
	}
	if len(args) != len(want) {
		t.Fatalf("args = %+v, want %d of them", args, len(want))
	}
}

func TestResolveBuildArgs_RefusalsNameTheArgumentAndTheFile(t *testing.T) {
	repo := newTestRepo(t, map[string]string{
		"go.mod":        "module example.com/x\n\ngo 1.22.5\n",
		"blank.txt":     "\n   \n",
		"script/check":  "#!/bin/sh\nexit 0\n",
		".ruby-version": "3.4.13\n",
	})
	commit := strings.TrimSpace(runGitT(t, repo, "rev-parse", "HEAD"))

	cases := map[string]struct {
		name   string
		source BuildArgSource
		want   string
	}{
		"a file the commit does not hold": {"GO_VERSION", BuildArgSource{File: "go.sum"}, "go.sum"},
		"a directory rather than a file":  {"GO_VERSION", BuildArgSource{File: "script"}, "script"},
		"a file with no non-empty line":   {"GO_VERSION", BuildArgSource{File: "blank.txt"}, "no non-empty line"},
		"a pattern that matches nothing": {"GO_VERSION", BuildArgSource{File: "go.mod", Pattern: `(?m)^ruby (\S+)$`},
			"matches nothing"},
		"a pattern that does not compile": {"GO_VERSION", BuildArgSource{File: "go.mod", Pattern: `go (\S+`},
			"not a valid regular expression"},
		"a pattern with no capture group": {"GO_VERSION", BuildArgSource{File: "go.mod", Pattern: `(?m)^go \S+$`},
			"exactly one"},
		"a pattern with two capture groups": {"GO_VERSION", BuildArgSource{File: "go.mod", Pattern: `(?m)^(go) (\S+)$`},
			"exactly one"},
		"a capture that swallows a line break": {"GO_VERSION", BuildArgSource{File: "go.mod", Pattern: `(?s)module (.+)`},
			"control character"},
		"no file at all":      {"GO_VERSION", BuildArgSource{}, "names no \"file\""},
		"an absolute path":    {"GO_VERSION", BuildArgSource{File: "/etc/hostname"}, "absolute path"},
		"a path climbing out": {"GO_VERSION", BuildArgSource{File: "../outside/.ruby-version"}, "repository root"},
		"a path with a colon": {"GO_VERSION", BuildArgSource{File: "a:b"}, "colon"},
		"a name docker will not take": {"GO-VERSION", BuildArgSource{File: ".ruby-version"},
			"letters, digits and underscores"},
	}
	for label, tc := range cases {
		_, err := ResolveBuildArgs(repo, commit, map[string]BuildArgSource{tc.name: tc.source})
		if err == nil {
			t.Fatalf("%s: expected a refusal, got none", label)
		}
		if !errors.Is(err, container.ErrBuildArgUnavailable) || !errors.Is(err, container.ErrProjectImageUnavailable) {
			t.Fatalf("%s: err = %v, want it to wrap container.ErrBuildArgUnavailable, which Run turns into a refusal and exit 2", label, err)
		}
		if !strings.Contains(err.Error(), tc.name) {
			t.Fatalf("%s: err = %q, want it to name the build argument", label, err)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %q, want it to say %q", label, err, tc.want)
		}
	}
}

// The whole file is read to find the value in it, so one too large to read
// is refused by name rather than being read anyway - a declared path aimed
// at a repository's large tracked asset must not make inspector itself the
// problem.
func TestResolveBuildArgs_RefusesAFileTooLargeToRead(t *testing.T) {
	repo := newTestRepo(t, map[string]string{
		"huge.txt": strings.Repeat("9", MaxBuildArgFileBytes+1) + "\n",
	})
	commit := strings.TrimSpace(runGitT(t, repo, "rev-parse", "HEAD"))

	_, err := ResolveBuildArgs(repo, commit, map[string]BuildArgSource{"V": {File: "huge.txt"}})
	if !errors.Is(err, container.ErrBuildArgUnavailable) {
		t.Fatalf("err = %v, want a refusal wrapping container.ErrBuildArgUnavailable", err)
	}
	if !strings.Contains(err.Error(), "huge.txt") || !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("err = %q, want it to name the file and say how large it is", err)
	}
}

// A tracked symbolic link reads as its own link text, never as the file it
// points at, so a declared path cannot reach anything the commit does not
// contain - and a link to a real secret resolves to a value that is
// obviously not a version rather than to its contents.
func TestResolveBuildArgs_ASymlinkCannotReadOutsideTheCommit(t *testing.T) {
	repo := newTestRepo(t, nil)
	runGitT(t, repo, "update-index", "--add", "--cacheinfo", "120000,"+
		blobOf(t, repo, "/etc/hostname")+",outside-link")
	runGitT(t, repo, "commit", "-q", "-m", "a tracked symlink")
	commit := strings.TrimSpace(runGitT(t, repo, "rev-parse", "HEAD"))

	args, err := ResolveBuildArgs(repo, commit, map[string]BuildArgSource{"LINKED": {File: "outside-link"}})
	if err != nil {
		t.Fatalf("resolving a symlink entry: %v", err)
	}
	if len(args) != 1 || args[0].Value != "/etc/hostname" {
		t.Fatalf("args = %+v, want the link's own text - the file it points at is outside the commit and must stay unread", args)
	}
}

// blobOf writes content into the repository's object database and returns
// its object name, for the index entries git has no plain-file command for.
func blobOf(t *testing.T, repo, content string) string {
	t.Helper()
	cmd := newValidationGit(repo, "hash-object", "-w", "--stdin")
	cmd.Stdin = strings.NewReader(content)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("writing a test blob: %v", err)
	}
	return strings.TrimSpace(string(out))
}
