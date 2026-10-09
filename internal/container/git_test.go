package container

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitImage is a small official library image whose whole purpose is
// "git plus a shell" - the git tests below need both. `alpine` (what
// every other container test here uses) carries no git at all, and
// `alpine/git`'s own ENTRYPOINT is `git`, so the `sh -c <command>`
// every check command runs as cannot be run in it.
const gitImage = "buildpack-deps:bookworm-scm"

// hostSecret is the credential-shaped value planted in the project's
// real git config. Nothing inside the container may ever see it.
const hostSecret = "inspector-test-host-token-must-not-leak"

// newWorktreeProject creates a git project with one commit, plants a
// credential-shaped value in that project's shared config the way a
// real checkout carries one, and adds a linked worktree of it. It
// returns the worktree - the repo root inspector would be pointed at,
// whose own .git is a pointer file naming a directory outside it - and
// the project's shared .git that pointer leads to.
func newWorktreeProject(t *testing.T) (worktree, commonGitDir string) {
	t.Helper()

	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolving the temp dir: %v", err)
	}
	project := filepath.Join(base, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatalf("creating the project dir: %v", err)
	}

	git(t, project, "init", "-q")
	git(t, project, "config", "user.email", "test@example.com")
	git(t, project, "config", "user.name", "Test")
	// Every shape of config-borne credential the sanitizer has to keep
	// out, not just a remote URL: a helper, an authorization header,
	// and a URL rewrite all live outside [remote] entirely.
	git(t, project, "config", "remote.origin.url", "https://user:"+hostSecret+"@example.com/project.git")
	git(t, project, "config", "credential.helper", "!f() { echo password="+hostSecret+"; }; f")
	git(t, project, "config", "http.https://example.com/.extraheader", "Authorization: Bearer "+hostSecret)
	git(t, project, "config", "url.https://user:"+hostSecret+"@example.com/.insteadOf", "https://example.com/")
	if err := os.WriteFile(filepath.Join(project, "file.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatalf("writing a file to commit: %v", err)
	}
	git(t, project, "add", "-A")
	git(t, project, "commit", "-q", "-m", "first commit")

	worktree = filepath.Join(base, "worktree")
	git(t, project, "worktree", "add", "-q", "--detach", worktree)

	pointer, err := os.ReadFile(filepath.Join(worktree, ".git"))
	if err != nil {
		t.Fatalf("the worktree's .git pointer file: %v", err)
	}
	if !strings.HasPrefix(string(pointer), "gitdir: ") {
		t.Fatalf("%q is not a worktree pointer file - this test's premise is gone", pointer)
	}
	return worktree, filepath.Join(project, ".git")
}

// git runs one git command in dir, with the host's own global and
// system configuration out of the way so these tests describe only the
// repositories they build themselves.
func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// runGitCheck runs command inside a container built exactly the way a
// real check command's is, for the repo root at worktree, and returns
// its combined output.
func runGitCheck(t *testing.T, worktree, command string) (string, error) {
	t.Helper()

	mounts, err := ResolveGitMounts(worktree)
	if err != nil {
		t.Fatalf("ResolveGitMounts(%q) = %v, want the mounts a linked worktree needs", worktree, err)
	}
	t.Cleanup(mounts.Cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := New(ctx, Run{
		RepoRoot:       worktree,
		Command:        command,
		Image:          gitImage,
		ReadOnlyMounts: mounts.Mounts,
	})
	defer cmd.Cleanup()
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	runErr := cmd.Run()
	t.Logf("in-container `%s`:\n%s", command, out.String())
	return out.String(), runErr
}

// TestContainerCannotReadHostGitConfig is a negative test: it attempts
// the forbidden act and must fail. The project's shared git config
// carries a credential, and mounting that directory as-is would hand it
// to whatever the check command runs. This fails if the real config is
// ever reachable inside the container.
func TestContainerCannotReadHostGitConfig(t *testing.T) {
	requireDocker(t)
	worktree, commonGitDir := newWorktreeProject(t)

	out, _ := runGitCheck(t, worktree, strings.Join([]string{
		"git config --get remote.origin.url",
		"git config --list",
		"cat " + filepath.Join(commonGitDir, "config"),
	}, "; "))

	// Proof the assertions below mean something: git really did read a
	// repository config in there, it just wasn't the real one.
	if !strings.Contains(out, "core.repositoryformatversion") {
		t.Fatalf("git read no repository config at all inside the container, so this test proves nothing:\n%s", out)
	}
	if strings.Contains(out, hostSecret) {
		t.Fatalf("the host's git credential is reachable inside the container:\n%s", out)
	}
	for _, forbidden := range []string{"remote.origin.url", "credential.helper", "extraheader", "insteadof"} {
		if strings.Contains(strings.ToLower(out), forbidden) {
			t.Fatalf("%q from the host's git config is visible inside the container:\n%s", forbidden, out)
		}
	}
}

// TestContainerCannotCommit is a negative test: it attempts the
// forbidden act and must fail. The git directory is mounted read-only,
// so a check command cannot commit its own work to keep it reachable
// after the container exits. This fails if that mount is ever writable.
func TestContainerCannotCommit(t *testing.T) {
	requireDocker(t)
	worktree, _ := newWorktreeProject(t)

	out, err := runGitCheck(t, worktree,
		"git -c user.email=t@example.com -c user.name=T commit --allow-empty -m 'should not be possible'")

	if err == nil {
		t.Fatalf("committing inside the container succeeded - the git directory is writable:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "read-only file system") {
		t.Fatalf("the commit failed, but not because the git directory is read-only:\n%s", out)
	}
}

// TestLinkedWorktreeGitIsReachable is the positive counterpart to the
// two negative tests above, not a substitute for either: it proves a
// check command that uses git works from a linked worktree, which is
// the defect issue #31 names.
func TestLinkedWorktreeGitIsReachable(t *testing.T) {
	requireDocker(t)
	worktree, _ := newWorktreeProject(t)

	out, err := runGitCheck(t, worktree, "git status --porcelain && git log --oneline")

	if err != nil {
		t.Fatalf("git failed inside the container from a linked worktree: %v\n%s", err, out)
	}
	if !strings.Contains(out, "first commit") {
		t.Fatalf("git log inside the container does not show the project's real history:\n%s", out)
	}
}

func TestResolveGitMounts_PlainRepoNeedsNothing(t *testing.T) {
	// An ordinary clone's .git is a directory inside the repo root, so
	// it is already inside the one mount that was always there.
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("creating a .git directory: %v", err)
	}

	mounts, err := ResolveGitMounts(dir)
	if err != nil {
		t.Fatalf("ResolveGitMounts(plain repo) = %v, want nil", err)
	}
	defer mounts.Cleanup()
	if len(mounts.Mounts) != 0 {
		t.Fatalf("ResolveGitMounts(plain repo) mounted %v, want nothing extra", mounts.Mounts)
	}
}

func TestResolveGitMounts_BrokenPointerRefuses(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /nowhere/at/all\n"), 0o644); err != nil {
		t.Fatalf("writing a dangling pointer file: %v", err)
	}

	_, err := ResolveGitMounts(dir)
	if err == nil {
		t.Fatal("ResolveGitMounts(dangling pointer) = nil, want a refusal rather than a container whose git is broken")
	}
	if !strings.Contains(err.Error(), "commondir") {
		t.Fatalf("ResolveGitMounts error = %v, want it to name what could not be read", err)
	}
}

func TestResolveGitMounts_MountsGitDirAtItsOwnPath(t *testing.T) {
	// git records the physical path in the pointer file, so the mount
	// has to land at exactly that path inside the container or git
	// follows the pointer to nothing.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("no git on PATH, skipping: %v", err)
	}
	worktree, commonGitDir := newWorktreeProject(t)

	mounts, err := ResolveGitMounts(worktree)
	if err != nil {
		t.Fatalf("ResolveGitMounts(%q) = %v", worktree, err)
	}
	defer mounts.Cleanup()

	if len(mounts.Mounts) != 2 {
		t.Fatalf("mounts = %v, want the git directory plus a sanitized config over its own config", mounts.Mounts)
	}
	if mounts.Mounts[0].Source != commonGitDir || mounts.Mounts[0].Target != commonGitDir {
		t.Fatalf("mounts[0] = %+v, want %q mounted at its own path", mounts.Mounts[0], commonGitDir)
	}
	wantConfigTarget := filepath.Join(commonGitDir, "config")
	if mounts.Mounts[1].Target != wantConfigTarget {
		t.Fatalf("mounts[1].Target = %q, want the real config's own path %q", mounts.Mounts[1].Target, wantConfigTarget)
	}
	if mounts.Mounts[1].Source == wantConfigTarget {
		t.Fatal("the config mount's source is the real config - it must be a sanitized throwaway copy")
	}
	sanitized, err := os.ReadFile(mounts.Mounts[1].Source)
	if err != nil {
		t.Fatalf("reading the sanitized config: %v", err)
	}
	if strings.Contains(string(sanitized), hostSecret) {
		t.Fatalf("the sanitized config carries the host credential:\n%s", sanitized)
	}

	mounts.Cleanup()
	if _, err := os.Stat(mounts.Mounts[1].Source); !os.IsNotExist(err) {
		t.Fatalf("Cleanup() left the sanitized config behind: %v", err)
	}
}

func TestSanitizeGitConfig(t *testing.T) {
	tests := []struct {
		name   string
		config string
		want   []string
		absent []string
	}{
		{
			name: "keeps core and drops every other section",
			config: "[core]\n\trepositoryformatversion = 0\n\tbare = false\n" +
				"[remote \"origin\"]\n\turl = https://user:token@example.com/p.git\n" +
				"[remote.origin]\n\turl = https://user:token@example.com/p.git\n" +
				"[credential]\n\thelper = store\n" +
				"[http \"https://example.com/\"]\n\textraheader = Authorization: Bearer token\n" +
				"[include]\n\tpath = /host/secrets.gitconfig\n",
			want:   []string{"[core]", "repositoryformatversion = 0", "bare = false"},
			absent: []string{"token", "remote", "credential", "extraheader", "include"},
		},
		{
			name:   "forwards allowlisted extension keys, on their own line or riding the header",
			config: "[core]\n\tbare = false\n[extensions] objectFormat = sha256\n\trefStorage = reftable\n",
			want:   []string{"[extensions]", "objectFormat = sha256", "refStorage = reftable"},
		},
		{
			name:   "keeps a core variable written on the header line",
			config: "[core] bare = false\n",
			want:   []string{"bare = false"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := sanitizeGitConfig(test.config, "config")
			if err != nil {
				t.Fatalf("sanitizeGitConfig() = %v, want nil", err)
			}
			for _, want := range test.want {
				if !strings.Contains(got, want) {
					t.Fatalf("sanitized config = %q, want it to contain %q", got, want)
				}
			}
			for _, absent := range test.absent {
				if strings.Contains(strings.ToLower(got), absent) {
					t.Fatalf("sanitized config = %q, want it to drop %q", got, absent)
				}
			}
		})
	}
}

func TestSanitizeGitConfig_RefusesWhatItCannotVouchFor(t *testing.T) {
	tests := []struct {
		name   string
		config string
		want   string
	}{
		{
			name:   "an extension key that is not allowlisted",
			config: "[core]\n\tbare = false\n[extensions]\n\tworktreeConfig = true\n",
			want:   "worktreeConfig",
		},
		{
			name:   "a refStorage naming a host location",
			config: "[extensions]\n\trefStorage = postgres://user:token@db/refs\n",
			want:   "refStorage",
		},
		{
			name:   "a subsectioned extensions header",
			config: "[extensions \"custom\"]\n\tsomething = 1\n",
			want:   "subsection",
		},
		{
			name:   "an extensions line that is not a plain key",
			config: "[extensions]\n\t= nonsense\n",
			want:   "cannot parse",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := sanitizeGitConfig(test.config, "config")
			if err == nil {
				t.Fatalf("sanitizeGitConfig() = %q, nil - want a refusal rather than a guess", got)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("sanitizeGitConfig() error = %v, want it to name %q", err, test.want)
			}
		})
	}
}
