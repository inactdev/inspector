package container

import (
	"errors"
	"strings"
	"testing"
)

func TestBuildArgs_NoneIsExactlyAsBefore(t *testing.T) {
	// A project that declares none has to produce the identical docker
	// command line, and the identical tag, it produced before this key
	// existed - no project may rebuild for a feature it does not use.
	dockerfile := []byte("FROM alpine\n")
	bases := []BaseImage{{Ref: "alpine", ID: "sha256:" + strings.Repeat("a", 64)}}
	for _, args := range [][]BuildArg{nil, {}} {
		if err := ValidateBuildArgs(args); err != nil {
			t.Fatalf("validating %v: %v", args, err)
		}
		command := buildCommandArgs("/repo/Dockerfile.inspector", "inspector-check:abc", RepoBuildContext+"=/repo", "/repo", nil, args)
		want := []string{"build", "-f", "/repo/Dockerfile.inspector", "-t", "inspector-check:abc", "--build-context", RepoBuildContext + "=/repo", "/repo"}
		if strings.Join(command, " ") != strings.Join(want, " ") {
			t.Fatalf("docker args = %v, want the unchanged %v", command, want)
		}
		if got := ProjectImageTag(dockerfile, bases, args); got != ProjectImageTag(dockerfile, bases, nil) {
			t.Fatalf("tag = %q for %v, want the tag a project declaring no build arguments has always had", got, args)
		}
	}
}

func TestBuildCommandArgs_PassesOneBuildArgPerDeclaredPair(t *testing.T) {
	args := []BuildArg{{Name: "GO_VERSION", Value: "1.22.5"}, {Name: "RUBY_VERSION", Value: "3.4.13"}}
	command := buildCommandArgs("/repo/Dockerfile.inspector", "inspector-check:abc", RepoBuildContext+"=/repo", "/repo", nil, args)
	line := strings.Join(command, " ")
	for _, arg := range args {
		if !strings.Contains(line, "--build-arg "+arg.Name+"="+arg.Value) {
			t.Fatalf("docker args = %v, want --build-arg %s=%s", command, arg.Name, arg.Value)
		}
	}
	// The build context stays the last argument, where docker wants it,
	// however many build arguments came before it.
	if command[len(command)-1] != "/repo" {
		t.Fatalf("docker args = %v, want the build context last", command)
	}
}

func TestProjectImageTag_FoldsInTheBuildArgValues(t *testing.T) {
	// This is the whole reason the values are in the tag: a Dockerfile
	// reading no build context is reused without building, and
	// `FROM ruby:${RUBY_VERSION}` is exactly such a Dockerfile - so
	// without this, a bumped .ruby-version would run the check in the
	// image built for the old one.
	dockerfile := []byte("ARG RUBY_VERSION\nFROM ruby:${RUBY_VERSION}\n")
	bases := []BaseImage{{Ref: "ruby:3.4.13", ID: "sha256:" + strings.Repeat("a", 64)}}
	tag := ProjectImageTag(dockerfile, bases, []BuildArg{{Name: "RUBY_VERSION", Value: "3.4.13"}})

	if tag != ProjectImageTag(dockerfile, bases, []BuildArg{{Name: "RUBY_VERSION", Value: "3.4.13"}}) {
		t.Fatal("the same build argument produced two different tags; the cached image could never be reused")
	}
	if tag == ProjectImageTag(dockerfile, bases, []BuildArg{{Name: "RUBY_VERSION", Value: "3.4.14"}}) {
		t.Fatal("a changed version produced the same tag; a check would run in the image built for the old one")
	}
	if tag == ProjectImageTag(dockerfile, bases, nil) {
		t.Fatal("dropping the build argument produced the same tag")
	}
	// A name and a value are hashed as a pair, so moving the boundary
	// between them cannot collide.
	if ProjectImageTag(dockerfile, bases, []BuildArg{{Name: "A", Value: "B=C"}}) ==
		ProjectImageTag(dockerfile, bases, []BuildArg{{Name: "A", Value: "B"}, {Name: "C", Value: ""}}) {
		t.Fatal("two different pair lists hashed the same")
	}
}

func TestParseDockerfile_ExpandsDeclaredBuildArgsInFrom(t *testing.T) {
	// The base inspector resolves, pulls, hashes into the tag and records
	// in the report has to be the image the line actually names - not the
	// unexpanded text, which is no image at all.
	args := []BuildArg{{Name: "RUBY_VERSION", Value: "3.4.13"}, {Name: "SUFFIX", Value: "slim"}}
	for name, dockerfile := range map[string]string{
		"braced": "ARG RUBY_VERSION\nFROM ruby:${RUBY_VERSION}\n",
		"bare":   "ARG RUBY_VERSION\nFROM ruby:$RUBY_VERSION\n",
		"with a platform flag and a stage name": "ARG RUBY_VERSION\n" +
			"FROM --platform=linux/amd64 ruby:${RUBY_VERSION} AS app\n",
	} {
		facts, err := parseDockerfile(dockerfile, args)
		if err != nil {
			t.Fatalf("%s: unexpected refusal: %v", name, err)
		}
		if strings.Join(facts.baseRefs, ",") != "ruby:3.4.13" {
			t.Fatalf("%s: baseRefs = %v, want the expanded ruby:3.4.13", name, facts.baseRefs)
		}
	}

	// More than one reference on a line, and a name that is a prefix of
	// nothing else, both resolve to one reference.
	facts, err := parseDockerfile("FROM ruby:${RUBY_VERSION}-${SUFFIX}\n", args)
	if err != nil {
		t.Fatalf("unexpected refusal: %v", err)
	}
	if strings.Join(facts.baseRefs, ",") != "ruby:3.4.13-slim" {
		t.Fatalf("baseRefs = %v, want ruby:3.4.13-slim", facts.baseRefs)
	}
}

func TestParseDockerfile_RefusesAFromItCannotExpand(t *testing.T) {
	// Refusing is the safe direction: the failure being avoided is
	// inspector resolving and hashing one base image while docker builds
	// the project from another.
	args := []BuildArg{{Name: "RUBY_VERSION", Value: "3.4.13"}}
	cases := map[string]string{
		"a variable no buildArgs entry declares":  "FROM ruby:${RUBY_TINY}\n",
		"a shell default this is not a shell for": "FROM ruby:${RUBY_VERSION:-3.3}\n",
		"an unterminated brace":                   "FROM ruby:${RUBY_VERSION\n",
		"a bare dollar":                           "FROM ruby:$\n",
	}
	for name, dockerfile := range cases {
		_, err := parseDockerfile(dockerfile, args)
		if err == nil {
			t.Fatalf("%s: expected a refusal, got none", name)
		}
		if !strings.Contains(err.Error(), "variable") || !strings.Contains(err.Error(), "buildArgs") {
			t.Fatalf("%s: error = %q, want it to name the variable problem and the buildArgs key that declares one", name, err)
		}
	}

	// And a value that itself carries a dollar sign cannot smuggle one
	// back in: docker would not expand it a second time, and inspector
	// will not resolve a reference it cannot read literally.
	if _, err := parseDockerfile("FROM ruby:${RUBY_VERSION}\n", []BuildArg{{Name: "RUBY_VERSION", Value: "$OTHER"}}); err == nil {
		t.Fatal("expected a refusal for a substituted value carrying a dollar sign, got none")
	}
}

func TestParseDockerfile_ExpandedFromStillReadsTheRepoContext(t *testing.T) {
	// Expansion happens before every comparison below it, so a reference
	// that expands to the repository context, to scratch, or to an earlier
	// stage is still read as one.
	facts, err := parseDockerfile("FROM ${TARGET}\n", []BuildArg{{Name: "TARGET", Value: RepoBuildContext}})
	if err != nil {
		t.Fatalf("unexpected refusal: %v", err)
	}
	if len(facts.baseRefs) != 0 {
		t.Fatalf("baseRefs = %v, want none: the repository context is no image and has no digest to resolve", facts.baseRefs)
	}
	if !facts.readsBuildContext {
		t.Fatal("readsBuildContext = false, want true: this build reads the repository under check, which the tag cannot see")
	}
}

func TestValidateBuildArgs_RefusesWhatItWillNotPass(t *testing.T) {
	cases := map[string]struct {
		args []BuildArg
		want string
	}{
		"a name carrying an equals sign": {[]BuildArg{{Name: "A=B", Value: "1"}}, "letters, digits and underscores"},
		"a name carrying a space":        {[]BuildArg{{Name: "A B", Value: "1"}}, "letters, digits and underscores"},
		"an empty name":                  {[]BuildArg{{Name: "", Value: "1"}}, "letters, digits and underscores"},
		"one name twice": {[]BuildArg{{Name: "V", Value: "1"}, {Name: "V", Value: "2"}},
			"declared more than once"},
		"an empty value":  {[]BuildArg{{Name: "V", Value: ""}}, "empty value"},
		"a newline value": {[]BuildArg{{Name: "V", Value: "1.0\n2.0"}}, "control character"},
		"a tab value":     {[]BuildArg{{Name: "V", Value: "1.0\t2.0"}}, "control character"},
		"an oversized value": {[]BuildArg{{Name: "V", Value: strings.Repeat("9", MaxBuildArgValueBytes+1)}},
			"past the"},
		"a value that is not UTF-8": {[]BuildArg{{Name: "V", Value: "\xff\xfe"}}, "UTF-8"},
	}
	for name, tc := range cases {
		err := ValidateBuildArgs(tc.args)
		if err == nil {
			t.Fatalf("%s: expected a refusal, got none", name)
		}
		if !errors.Is(err, ErrBuildArgUnavailable) || !errors.Is(err, ErrProjectImageUnavailable) {
			t.Fatalf("%s: err = %v, want it to wrap ErrBuildArgUnavailable and so ErrProjectImageUnavailable, which every caller turns into a refusal", name, err)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %q, want it to say %q", name, err, tc.want)
		}
	}

	// A plausible version string is accepted whole - this must not become a
	// version-syntax opinion.
	for _, value := range []string{"3.4.13", "1.22.5-slim", "v2", "sha256:" + strings.Repeat("a", 64), "2.5.6.rc1"} {
		if err := ValidateBuildArgs([]BuildArg{{Name: "V", Value: value}}); err != nil {
			t.Fatalf("value %q was refused: %v", value, err)
		}
	}
}
