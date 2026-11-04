package container

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The value is the one thing that must never leave the environment it was
// read from, so every test here that needs one uses this and then proves
// it appears nowhere.
const fakeSecretValue = "inspector-fake-secret-value-do-not-leak-6b1f"

func TestResolveBuildSecrets_NoneIsExactlyAsBefore(t *testing.T) {
	// A project that declares none has to produce the identical docker
	// command line it produced before this key existed - this key may add
	// nothing to a build that did not ask for it.
	for _, names := range [][]string{nil, {}} {
		resolved, err := resolveBuildSecrets(names)
		if err != nil {
			t.Fatalf("resolving %v: %v", names, err)
		}
		if len(resolved) != 0 {
			t.Fatalf("resolved = %v, want nothing", resolved)
		}
		args := buildArgs("/repo/Dockerfile.inspector", "inspector-check:abc", RepoBuildContext+"=/repo", "/repo", resolved)
		want := []string{"build", "-f", "/repo/Dockerfile.inspector", "-t", "inspector-check:abc", "--build-context", RepoBuildContext + "=/repo", "/repo"}
		if strings.Join(args, " ") != strings.Join(want, " ") {
			t.Fatalf("docker args = %v, want the unchanged %v", args, want)
		}
	}
}

func TestResolveBuildSecrets_PassesNamesAndNeverValues(t *testing.T) {
	t.Setenv("INSPECTOR_TEST_SECRET_ONE", fakeSecretValue)
	t.Setenv("INSPECTOR_TEST_SECRET_TWO", fakeSecretValue)

	resolved, err := resolveBuildSecrets([]string{"INSPECTOR_TEST_SECRET_ONE", "INSPECTOR_TEST_SECRET_TWO"})
	if err != nil {
		t.Fatalf("resolving two declared secrets: %v", err)
	}
	args := buildArgs("/repo/Dockerfile.inspector", "inspector-check:abc", RepoBuildContext+"=/repo", "/repo", resolved)
	line := strings.Join(args, " ")
	for _, name := range resolved {
		if !strings.Contains(line, "--secret id="+name+",env="+name) {
			t.Fatalf("docker args = %v, want --secret naming %s as an environment variable docker reads itself", args, name)
		}
	}
	// The whole point: the credential travels from the environment into
	// BuildKit without passing through anything inspector writes.
	if strings.Contains(line, fakeSecretValue) {
		t.Fatalf("docker args = %v, want the value nowhere on the command line - a command line is readable by every process on the host", args)
	}
	// And the build context stays the last argument, where docker wants
	// it, however many secrets came before it.
	if args[len(args)-1] != "/repo" {
		t.Fatalf("docker args = %v, want the build context last", args)
	}
}

func TestResolveBuildSecrets_RefusesNamesItWillNotPass(t *testing.T) {
	// A name goes into a comma-separated flag value, so anything outside
	// letters, digits and underscores could change which flag docker
	// reads. Refused by name, never escaped or dropped.
	for _, name := range []string{
		"", "BUNDLE,OTHER", "BUNDLE=x", "BUNDLE TOKEN", "BUNDLE-TOKEN", "BUNDLE\nOTHER", "BUNDLE$TOKEN", "id=x,env=y",
	} {
		t.Setenv("BUNDLE_TOKEN", fakeSecretValue)
		err := ValidateBuildSecrets([]string{name})
		if !errors.Is(err, ErrBuildSecretUnavailable) {
			t.Fatalf("%q: err = %v, want it to wrap ErrBuildSecretUnavailable", name, err)
		}
		if !errors.Is(err, ErrProjectImageUnavailable) {
			t.Fatalf("%q: err = %v, want it to wrap ErrProjectImageUnavailable too, so a caller knowing only that sentinel still refuses", name, err)
		}
		// Quoted, which is also how the refusal has to print it: a name
		// carrying a newline is exactly the kind of entry whose shape a
		// reader cannot see otherwise.
		if !strings.Contains(err.Error(), strconv.Quote(name)) {
			t.Fatalf("%q: err = %q, want the refusal to name the entry it refused", name, err)
		}
	}
}

func TestResolveBuildSecrets_UnsetOrEmptyIsRefusedByName(t *testing.T) {
	const name = "INSPECTOR_TEST_SECRET_ABSENT"
	// Empty counts as missing on purpose and is the case that actually
	// happens: a CI secret that does not exist arrives as an empty
	// variable rather than an unset one, the same ruling gate-sync.sh
	// already makes.
	cases := map[string]func(){
		// t.Setenv first, so its cleanup restores whatever this machine
		// really has once the test is over.
		"unset":      func() { t.Setenv(name, ""); os.Unsetenv(name) },
		"empty":      func() { t.Setenv(name, "") },
		"whitespace": func() { t.Setenv(name, "  \n") },
	}
	for label, prepare := range cases {
		prepare()
		err := ValidateBuildSecrets([]string{name})
		if !errors.Is(err, ErrBuildSecretUnavailable) {
			t.Fatalf("%s: err = %v, want it to wrap ErrBuildSecretUnavailable", label, err)
		}
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("%s: err = %q, want the refusal to name the variable to set", label, err)
		}
	}
}

func TestResolveBuildSecrets_CollapsesRepeats(t *testing.T) {
	// Twice in the list says the same thing once. One flag per name,
	// first-mentioned order kept: two --secret flags carrying one id is
	// not behavior docker documents, so inspector does not rely on it.
	t.Setenv("INSPECTOR_TEST_SECRET_ONE", fakeSecretValue)
	t.Setenv("INSPECTOR_TEST_SECRET_TWO", fakeSecretValue)

	resolved, err := resolveBuildSecrets([]string{"INSPECTOR_TEST_SECRET_TWO", "INSPECTOR_TEST_SECRET_ONE", "INSPECTOR_TEST_SECRET_TWO"})
	if err != nil {
		t.Fatalf("resolving a list with a repeat: %v", err)
	}
	want := "INSPECTOR_TEST_SECRET_TWO,INSPECTOR_TEST_SECRET_ONE"
	if strings.Join(resolved, ",") != want {
		t.Fatalf("resolved = %v, want %s", resolved, want)
	}
}
