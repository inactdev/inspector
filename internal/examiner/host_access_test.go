package examiner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/inactdev/inspector/internal/container"
)

func TestRuntimeIsolationNeverOptsOutOfTheDaemonsUserNamespace(t *testing.T) {
	// `--userns host` would discard whatever user namespace the daemon
	// puts the container in, which on a `userns-remap` host is the
	// isolation its administrator chose. The examiner's whole purpose is
	// a sealed runtime, so it honors that configuration and bridges the
	// resulting host identity instead (issue #32). This is the guard
	// against the flag coming back as a quick fix.
	for name, args := range map[string][]string{
		"examination":         newContainerCommand(context.Background(), containerOptions{InputDir: t.TempDir(), OutputDir: t.TempDir(), AppURL: "http://app:8080", Model: "test-model", Network: "none", User: "1000:1000"}, testRuntimeImageID).Args,
		"fingerprint probe":   newRuntimeIdentityCommand(context.Background(), "1000:1000", testRuntimeImageID).Args,
		"host identity probe": newHostIdentityCommand(context.Background(), "1000:1000", testRuntimeImageID).Args,
	} {
		joined := strings.Join(args, "\x00")
		if strings.Contains(joined, "--userns") {
			t.Fatalf("%s command opts out of the daemon's user namespace: %q", name, args)
		}
		if !strings.Contains(joined, "--user\x001000:1000") {
			t.Fatalf("%s command = %q, want the resolved runtime user", name, args)
		}
	}
}

func TestHostIdentityProbeIsIsolatedExactlyLikeTheExamination(t *testing.T) {
	// The probe measures which host user the examination will run as, and
	// `--user` plus the daemon's user-namespace configuration are what
	// decide that. A probe isolated differently from the real run would
	// measure the wrong identity and the grant would name the wrong host
	// account, so both are built from runtimeIsolationArgs.
	probe := newHostIdentityCommand(context.Background(), "501:20", testRuntimeImageID)
	defer probe.Cleanup()
	joined := strings.Join(probe.Args, "\x00")
	for _, want := range []string{
		"--pull\x00never", "--read-only", "--user\x00501:20", "--cap-drop\x00ALL",
		"--security-opt\x00no-new-privileges", "--network\x00none",
		"--entrypoint\x00/usr/local/bin/examiner-agent\x00" + testRuntimeImageID + "\x00--host-identity",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("host identity probe = %q, want %q", probe.Args, want)
		}
	}
	// It must not be handed any staged material: it answers one question
	// about the runtime and nothing about this examination.
	if strings.Contains(joined, "--mount") || strings.Contains(joined, "--env") {
		t.Fatalf("host identity probe = %q, want no mounts and no forwarded environment", probe.Args)
	}
}

func TestParseHostIdentity(t *testing.T) {
	uid, gid, err := parseHostIdentity(" 231072:231072\n")
	if err != nil {
		t.Fatalf("parseHostIdentity = %v", err)
	}
	if uid != 231072 || gid != 231072 {
		t.Fatalf("parseHostIdentity = %d:%d, want 231072:231072", uid, gid)
	}
	// Strict on purpose: this number decides which host user is granted
	// access to the staged request and verdict, so anything unreadable is
	// an error rather than a default.
	for name, output := range map[string]string{
		"empty":           "",
		"one field":       "231072\n",
		"three fields":    "1:2:3\n",
		"not a number":    "root:root\n",
		"negative uid":    "-1:0\n",
		"negative gid":    "0:-1\n",
		"trailing banner": "231072:231072\nsomething else\n",
	} {
		if _, _, err := parseHostIdentity(output); err == nil {
			t.Fatalf("%s: parseHostIdentity(%q) returned no error", name, output)
		}
	}
}

func TestInputFileNamesCoversEveryStagedInput(t *testing.T) {
	// A document PrepareInputs writes but inputFileNames omits is one the
	// container cannot read under a remapping daemon - the exact defect
	// issue #32 fixes, and invisible until someone runs on such a host.
	prepared, err := PrepareInputs(writableExampleInputs(t))
	if err != nil {
		t.Fatalf("preparing inputs: %v", err)
	}
	defer prepared.Cleanup()

	entries, err := os.ReadDir(prepared.Dir)
	if err != nil {
		t.Fatalf("reading the sealed input directory: %v", err)
	}
	staged := map[string]struct{}{}
	for _, entry := range entries {
		staged[entry.Name()] = struct{}{}
	}
	for _, name := range inputFileNames() {
		if _, ok := staged[name]; !ok {
			t.Fatalf("inputFileNames names %q, which PrepareInputs does not stage", name)
		}
		delete(staged, name)
	}
	if len(staged) != 0 {
		t.Fatalf("PrepareInputs staged %v, which inputFileNames does not name - the container could not read them under userns-remap", staged)
	}
}

func TestGrantStagedAccess_GrantsNothingWhenTheIdentityAlreadyMatches(t *testing.T) {
	// The ordinary case, and the one this must not disturb: on a daemon
	// that remaps nothing, the container runs as the host user that
	// already owns the staged paths, so they are left exactly as
	// PrepareInputs and prepareOutput made them.
	prepared, err := PrepareInputs(writableExampleInputs(t))
	if err != nil {
		t.Fatalf("preparing inputs: %v", err)
	}
	defer prepared.Cleanup()
	before := map[string]os.FileMode{}
	for _, name := range inputFileNames() {
		info, err := os.Stat(filepath.Join(prepared.Dir, name))
		if err != nil {
			t.Fatalf("reading staged input %q: %v", name, err)
		}
		before[name] = info.Mode().Perm()
	}

	if err := applyStagedGrant(prepared, t.TempDir(), filepath.Join(t.TempDir(), VerdictName), os.Getuid()); err != nil {
		t.Fatalf("applyStagedGrant for inspector's own uid = %v, want no work and no error", err)
	}

	for name, mode := range before {
		info, err := os.Stat(filepath.Join(prepared.Dir, name))
		if err != nil {
			t.Fatalf("re-reading staged input %q: %v", name, err)
		}
		if info.Mode().Perm() != mode {
			t.Fatalf("staged input %q changed from %04o to %04o with nothing to grant", name, mode, info.Mode().Perm())
		}
	}
}

func TestGrantStagedAccess_GivesTheRemappedUidExactlyWhatItNeeds(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("staged access is only granted on Linux hosts, skipping on %s", runtime.GOOS)
	}
	// 231072 is the remap base in Docker's own userns-remap
	// documentation, standing in here for the host identity a remapping
	// daemon would hand the container. This is the whole permission
	// layout TestExaminerStagedAccessUnderUsernsRemap verifies against a
	// live daemon, asserted without needing one.
	const remapped = 231072
	prepared, err := PrepareInputs(writableExampleInputs(t))
	if err != nil {
		t.Fatalf("preparing inputs: %v", err)
	}
	defer prepared.Cleanup()
	outputDir, verdictPath, cleanupOutput, err := prepareOutput()
	if err != nil {
		t.Fatalf("preparing the verdict output: %v", err)
	}
	defer cleanupOutput()

	if err := applyStagedGrant(prepared, outputDir, verdictPath, remapped); err != nil {
		if errors.Is(err, container.ErrAccessGrantUnsupported) {
			t.Skipf("POSIX ACLs are unavailable on this test file system, skipping: %v", err)
		}
		t.Fatalf("applyStagedGrant = %v", err)
	}

	want := map[string]container.Perm{
		// Traversal only on both directories: the agent opens fixed
		// names, so it must not be able to enumerate either one.
		prepared.Dir: container.Execute,
		outputDir:    container.Execute,
		// Write only, on the file rather than its directory, so the
		// container can fill in the verdict and cannot create, replace or
		// unlink anything beside it.
		verdictPath: container.Write,
	}
	for _, path := range stagedInputPaths(prepared) {
		want[path] = container.Read
	}
	for path, perm := range want {
		access, err := container.AccessOf(path)
		if err != nil {
			t.Fatalf("reading who can reach %q: %v", path, err)
		}
		if got := access.Users[remapped]; got != perm {
			t.Fatalf("%q grants the remapped uid %s, want exactly %s", path, got, perm)
		}
		if len(access.Users) != 1 {
			t.Fatalf("%q grants %d individual uids, want only the container's host identity", path, len(access.Users))
		}
		if access.Group != 0 || access.Other != 0 {
			t.Fatalf("%q became reachable by other users on the host: group %s, other %s", path, access.Group, access.Other)
		}
	}
}

func TestGrantStagedAccess_RefusesWhenTheStagedPathsCannotBeReachedAtAll(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("staged access is only granted on Linux hosts, skipping on %s", runtime.GOOS)
	}
	// A private TMPDIR puts the staging directories under one the
	// container's host user cannot traverse, which no grant on the staged
	// paths themselves can fix. Refused by name rather than worked around
	// by granting traversal up a chain of directories inspector does not
	// own, and rather than left to surface as the agent failing to read
	// its own inputs.
	private := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatalf("creating the private temporary directory: %v", err)
	}
	t.Setenv("TMPDIR", private)

	prepared, err := PrepareInputs(writableExampleInputs(t))
	if err != nil {
		t.Fatalf("preparing inputs: %v", err)
	}
	defer prepared.Cleanup()
	outputDir, verdictPath, cleanupOutput, err := prepareOutput()
	if err != nil {
		t.Fatalf("preparing the verdict output: %v", err)
	}
	defer cleanupOutput()

	err = applyStagedGrant(prepared, outputDir, verdictPath, 231072)
	if !errors.Is(err, container.ErrAccessGrantUnsupported) {
		t.Fatalf("applyStagedGrant under a private TMPDIR = %v, want ErrAccessGrantUnsupported", err)
	}
	if !strings.Contains(err.Error(), "TMPDIR") {
		t.Fatalf("refusal = %q, want it to name TMPDIR so the person running it can act on it", err)
	}
	access, accessErr := container.AccessOf(filepath.Join(prepared.Dir, InputRequestName))
	if accessErr != nil {
		t.Fatalf("reading who can reach the staged request: %v", accessErr)
	}
	if len(access.Users) != 0 {
		t.Fatalf("staged request granted %v despite the refusal", access.Users)
	}
}

func TestGrantStagedAccess_ReadsTheIdentityFromTheContainerAndGrantsIt(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("staged access is only granted on Linux hosts, skipping on %s", runtime.GOOS)
	}
	// The whole host-side flow, with a stand-in for the daemon: ask the
	// container which host user it runs as, then grant exactly that one.
	// A real remapping daemon is what TestExaminerStagedAccessUnderUsernsRemap
	// needs; this proves the resolve-and-grant path itself.
	const remapped = 231072
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	script := "#!/bin/sh\n" +
		"case \" $* \" in *\" --host-identity \"*) printf '%s\\n' '231072:231072'; exit 0;; esac\n" +
		"exit 99\n"
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatalf("writing fake docker: %v", err)
	}
	t.Setenv("PATH", dir)

	prepared, err := PrepareInputs(writableExampleInputs(t))
	if err != nil {
		t.Fatalf("preparing inputs: %v", err)
	}
	defer prepared.Cleanup()
	outputDir, verdictPath, cleanupOutput, err := prepareOutput()
	if err != nil {
		t.Fatalf("preparing the verdict output: %v", err)
	}
	defer cleanupOutput()

	if err := grantStagedAccess(context.Background(), prepared, outputDir, verdictPath, "1000:1000", testRuntimeImageID); err != nil {
		if errors.Is(err, container.ErrAccessGrantUnsupported) {
			t.Skipf("POSIX ACLs are unavailable on this test file system, skipping: %v", err)
		}
		t.Fatalf("grantStagedAccess = %v", err)
	}

	access, err := container.AccessOf(filepath.Join(prepared.Dir, InputRequestName))
	if err != nil {
		t.Fatalf("reading who can reach the staged request: %v", err)
	}
	if access.Users[remapped] != container.Read || len(access.Users) != 1 {
		t.Fatalf("staged request grants %v, want read for exactly the uid the container reported", access.Users)
	}
}

func TestGrantStagedAccess_RefusesAnUnreadableIdentity(t *testing.T) {
	// Nothing is granted on a guess. An unparseable or missing answer has
	// to refuse: the number decides which real host account gains access
	// to the request, guidebook and verdict.
	for name, answer := range map[string]string{
		"nonsense":  "printf 'who knows\\n'; exit 0\n",
		"no answer": "exit 0\n",
		"failure":   "printf 'boom\\n' >&2; exit 1\n",
	} {
		dir := t.TempDir()
		docker := filepath.Join(dir, "docker")
		if err := os.WriteFile(docker, []byte("#!/bin/sh\n"+answer), 0o700); err != nil {
			t.Fatalf("%s: writing fake docker: %v", name, err)
		}
		t.Setenv("PATH", dir)

		prepared, err := PrepareInputs(writableExampleInputs(t))
		if err != nil {
			t.Fatalf("%s: preparing inputs: %v", name, err)
		}
		outputDir, verdictPath, cleanupOutput, err := prepareOutput()
		if err != nil {
			prepared.Cleanup()
			t.Fatalf("%s: preparing the verdict output: %v", name, err)
		}
		err = grantStagedAccess(context.Background(), prepared, outputDir, verdictPath, "1000:1000", testRuntimeImageID)
		cleanupOutput()
		prepared.Cleanup()
		if err == nil {
			t.Fatalf("%s: grantStagedAccess returned no error for an unreadable host identity", name)
		}
	}
}

func TestVerdictIsWrittenThroughTheFileInspectorStaged(t *testing.T) {
	// The verdict has to come back through the inode inspector created,
	// not a fresh one renamed over it. Under a remapping daemon a file
	// the container created itself is owned by the container's host user
	// with an owner-only mode, and inspector - neither its owner nor able
	// to add an ACL entry to it - could never read it back (issue #32).
	outputDir, verdictPath, cleanupOutput, err := prepareOutput()
	if err != nil {
		t.Fatalf("preparing the verdict output: %v", err)
	}
	defer cleanupOutput()
	staged, err := os.Stat(verdictPath)
	if err != nil {
		t.Fatalf("reading the staged verdict: %v", err)
	}

	if err := writeResult(outputDir, Result{Kind: Green, Message: "written in place"}); err != nil {
		t.Fatalf("writing the verdict: %v", err)
	}

	published, err := os.Stat(verdictPath)
	if err != nil {
		t.Fatalf("reading the published verdict: %v", err)
	}
	if !os.SameFile(staged, published) {
		t.Fatal("the verdict was published as a different file than the one inspector staged, which inspector could not read back under userns-remap")
	}
	if published.Mode().Perm() != 0o600 {
		t.Fatalf("published verdict mode = %04o, want the owner-only mode inspector staged", published.Mode().Perm())
	}
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		t.Fatalf("reading the verdict output directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != VerdictName {
		t.Fatalf("verdict output directory holds %v, want only %q - the container is granted no permission to create anything beside it", entries, VerdictName)
	}
	result, err := readRuntimeResult(verdictPath)
	if err != nil {
		t.Fatalf("reading back the verdict: %v", err)
	}
	if result.Message != "written in place" {
		t.Fatalf("verdict = %#v, want the written result", result)
	}
}

// TestExaminerStagedAccessUnderUsernsRemap is the live verification issue
// #32 asks for: that the staged inputs and verdict are reachable by the
// container's own remapped host identity, and by no other user on the
// host. It needs a Docker daemon actually configured with
// `userns-remap`, so it skips loudly - the same tolerance the rest of
// this suite extends to a machine with no runtime at all.
//
// To run it, point DOCKER_HOST at a daemon started with
// `{"userns-remap": "default"}` in its daemon.json, with the examiner
// runtime image built against it.
func TestExaminerStagedAccessUnderUsernsRemap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	requireUsernsRemapDaemon(t, ctx)

	user, err := localDockerRuntimeUser(ctx)
	if err != nil {
		t.Skipf("no usable local Docker runtime, skipping: %v", err)
	}
	runtimeImage, err := ensureRuntimeImage(ctx, user)
	if err != nil {
		t.Skipf("local examiner runtime is unavailable, skipping: %v", err)
	}

	uid, err := containerHostUID(ctx, user, runtimeImage)
	if err != nil {
		t.Fatalf("reading the examiner container's host identity: %v", err)
	}
	if uid == os.Getuid() {
		t.Fatalf("container host uid = %d, the same as inspector's own: this daemon reports userns-remap but does not remap the examiner container, so the test proves nothing", uid)
	}

	prepared, err := PrepareInputs(writableExampleInputs(t))
	if err != nil {
		t.Fatalf("preparing inputs: %v", err)
	}
	defer prepared.Cleanup()
	outputDir, verdictPath, cleanupOutput, err := prepareOutput()
	if err != nil {
		t.Fatalf("preparing the verdict output: %v", err)
	}
	defer cleanupOutput()

	if err := grantStagedAccess(ctx, prepared, outputDir, verdictPath, user, runtimeImage); err != nil {
		t.Fatalf("granting the examiner container access to its staged resources: %v", err)
	}

	// The container reads every input and writes the verdict through the
	// file inspector staged, which is the whole end-to-end permission
	// path an examination depends on.
	script := "set -e\n"
	for _, name := range inputFileNames() {
		script += "cat /examiner/input/" + name + " > /dev/null\n"
	}
	script += "printf 'reached' > /examiner/output/" + VerdictName + "\n"
	if output, err := runInStagedContainer(ctx, user, runtimeImage, prepared.Dir, outputDir, script); err != nil {
		t.Fatalf("the remapped container could not use its staged resources: %v: %s", err, output)
	}

	written, err := os.ReadFile(verdictPath)
	if err != nil {
		t.Fatalf("reading back the verdict the remapped container wrote: %v", err)
	}
	if string(written) != "reached" {
		t.Fatalf("verdict = %q, want the bytes the remapped container wrote", written)
	}

	// The second half of the definition of done: nothing became readable
	// to other users on the host.
	for _, path := range append([]string{prepared.Dir, outputDir, verdictPath}, stagedInputPaths(prepared)...) {
		assertNoOtherUserAccess(t, path)
	}
}

func requireUsernsRemapDaemon(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed, so there is no userns-remap daemon to verify against; skipping")
	}
	output, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{json .SecurityOptions}}").Output()
	if err != nil {
		t.Skipf("no reachable Docker daemon to check for userns-remap, skipping: %v", err)
	}
	var securityOptions []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(output))), &securityOptions); err != nil {
		t.Skipf("Docker reported unreadable security options, skipping: %v: %s", err, output)
	}
	for _, option := range securityOptions {
		for _, field := range strings.Split(option, ",") {
			// The daemon appends this precisely when its id mapping moves
			// container root off host uid 0, which is what userns-remap
			// does and the only thing this test can verify against.
			if field == "userns" || field == "name=userns" {
				return
			}
		}
	}
	t.Skipf("this Docker daemon does not have userns-remap enabled (security options %v), so issue #32's fix cannot be verified live here; start a daemon with {\"userns-remap\": \"default\"} and point DOCKER_HOST at it to run this test", securityOptions)
}

func stagedInputPaths(prepared PreparedInputs) []string {
	var paths []string
	for _, name := range inputFileNames() {
		paths = append(paths, filepath.Join(prepared.Dir, name))
	}
	return paths
}

// assertNoOtherUserAccess is the second half of issue #32's definition
// of done. It reads the path's effective access rather than its mode,
// because a path carrying a POSIX ACL reports the ACL's mask in its
// st_mode group bits - so the mode of a correctly granted input file
// looks group-readable while no group member can read it.
func assertNoOtherUserAccess(t *testing.T, path string) {
	t.Helper()
	access, err := container.AccessOf(path)
	if err != nil {
		t.Fatalf("reading who can reach %q: %v", path, err)
	}
	if access.Other != 0 {
		t.Fatalf("%q gives every other account on this host %s access", path, access.Other)
	}
	if access.Group != 0 {
		t.Fatalf("%q gives its group %s access", path, access.Group)
	}
	if len(access.Users) != 1 {
		t.Fatalf("%q grants %d individual uids, want exactly the examiner container's own host identity", path, len(access.Users))
	}
}

// runInStagedContainer drives the staged input and output mounts from a
// container isolated exactly as the examination is. It runs a shell
// rather than the agent because what is under test is the permission
// path - whether the remapped host identity can read each input and
// write the verdict - and the agent would additionally need a model
// vendor and a running app to reach that point.
func runInStagedContainer(ctx context.Context, user, runtimeImageID, inputDir, outputDir, script string) (string, error) {
	args := runtimeIsolationArgs(user)
	args = append(args,
		"--mount", "type=bind,src="+inputDir+",dst=/examiner/input,readonly",
		"--mount", "type=bind,src="+outputDir+",dst=/examiner/output",
		"--workdir", "/examiner",
		"--network", "none",
		"--entrypoint", "/bin/sh",
		runtimeImageID,
		"-c", script,
	)
	cmd := container.NewCommand(ctx, "inspector-examiner-staging-test-", args...)
	defer cmd.Cleanup()
	var output strings.Builder
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	return output.String(), err
}

func writableExampleInputs(t *testing.T) Inputs {
	t.Helper()
	paths := writeInputFixture(t, t.TempDir())
	return Inputs{
		RequestPath: paths[InputRequestName], FeatureMapPath: paths[InputFeatureMapName],
		AlwaysTruePath: paths[InputAlwaysTrueName], ChangedFilesPath: paths[InputChangedFilesName],
		BaseTestsPath: paths[InputBaseTestsName],
	}
}
