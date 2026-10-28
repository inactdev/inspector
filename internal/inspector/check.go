package inspector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/inactdev/inspector/internal/container"
)

// CheckResult is the outcome of actually running the project's check
// command.
type CheckResult struct {
	ExitCode int
	// Signal is set when the check command was terminated by a signal
	// (e.g. the OOM killer, an external kill, inspector's own timeout)
	// rather than exiting on its own - detected two ways, since a
	// signal-killed process only sometimes looks like one. A directly
	// signaled process reports it through the OS wait status, and
	// ExitCode is -1 in that case (Go's os/exec convention, not a real
	// exit status). But `sh -c "a && b"` forks a child per command and
	// survives a killed one, exiting normally with 128+N - the shell's
	// own convention for "my child died from signal N" - so Signal can
	// also be set with ExitCode holding that 128+N value rather than
	// -1. Either way, callers must check Signal rather than assume a
	// nonzero ExitCode is the process's own verdict on the code.
	Signal string
	// TimedOut is set when RunCheck itself killed the command for
	// exceeding timeout, as distinct from some other signal (Signal is
	// also set in this case, since killing it is how the timeout is
	// enforced, but TimedOut identifies who pulled the trigger and why).
	TimedOut bool
	// Workspace is the throwaway writable area that stood in for a
	// writable repo while the check ran (issue #27) - everything the
	// check command wrote is in there, and nothing of it reached the
	// repository. The caller owns it from here: discard it with
	// Workspace.Cleanup() once this run's report is written, which is
	// the lifetime the ruling gives it. A zero Workspace, which is what
	// every error return carries, is safe to Cleanup.
	Workspace container.Workspace
	// KillFailed is set when the deadline fired and docker then refused
	// to stop the container, so inspector cannot claim the check command
	// is no longer running against the repo. It is additive reporting
	// only - classifyResult neither sets nor reads it, and no outcome
	// depends on it - but a caller phrasing a timeout to a human must
	// check it rather than say the check was killed when it may not have
	// been.
	KillFailed bool
	// Output is what the check command printed, bounded to
	// MaxOutputBytes plus the one marker line that stands where the
	// dropped middle was (issue #12). Everything the command printed
	// still reached the caller's own stdout and stderr live and in full;
	// this is the copy inspector keeps.
	Output string
	// OutputDroppedBytes is how many bytes of output Output does not
	// hold, and zero whenever it holds all of them. It is what lets a
	// report say plainly that output was cut rather than leaving a
	// reader to guess from its size. OutputTotalBytes is how much the
	// check command printed in total, which is the number that says
	// whether a project's own suite is as chatty as it meant to be.
	OutputDroppedBytes int64
	OutputTotalBytes   int64
}

// The bound on what one run keeps of its check command's output, ruled
// 2026-10-28 for issue #12 and fixed on purpose: no config key and no
// flag, because a project that can raise the cap can bring back the
// unbounded run this exists to prevent. Head and tail, not the first
// MaxOutputBytes: the reason a check failed is nearly always the last
// thing it printed, while the first thing it printed is what says which
// command and toolchain ran. See
// notes/20261028-bound-the-captured-output-and-prune-old-runs.md.
const (
	// MaxOutputBytes is the total kept per run, in memory and on disk
	// alike.
	MaxOutputBytes = 256 << 10
	// OutputHeadBytes is how much of the start is kept.
	OutputHeadBytes = 64 << 10
	// OutputTailBytes is how much of the end is kept.
	OutputTailBytes = MaxOutputBytes - OutputHeadBytes
)

// RunCheck runs command inside a fresh container built from image,
// working from container.WorkspaceDir, where repoRoot's tracked
// top-level paths are mounted read-only over a throwaway writable
// workspace - so the check command can install, build and write
// reports into its own tree without being able to modify the code it
// is being judged on (issue #27; see container.ResolveWorkspace) - plus,
// read-only, whatever container.ResolveGitMounts says the repo needs
// for git to work in there, and nothing else on the host - killing the
// container if it has not finished within timeout. The returned
// CheckResult carries that workspace, which the caller discards once
// this run's report is written. The container is bounded by
// limits (see container.Limits and its Default* constants), runs as
// the host user ResolveUser resolves rather than the image's own
// default, and is denied network access with no way to ask for one
// (issue #47): image is built from the project's own committed
// Dockerfile, so its dependencies are already in there. Output is
// streamed to stdout/stderr live and in full, and also captured for the
// local report - that captured copy, and only it, is bounded to
// MaxOutputBytes, keeping the head and the tail with a marker line
// naming the dropped middle (issue #12). The two streams are written
// from different goroutines, but every write - to the capture buffer and
// to the caller's writers alike - is serialized on one lock, so passing
// the same writer for both stdout and stderr is safe. The returned error
// is non-nil only for infrastructure failures (e.g. docker could not be
// started, the image could not be run at all, the repo's git directory
// could not be made available in there, or the host user to run as could
// not be resolved) - a failing check is a
// normal CheckResult with a non-zero ExitCode, not a Go error. Callers
// are expected to have already confirmed a usable runtime via
// container.EnsureAvailable - RunCheck itself does not check again, so
// its own failures here are the rarer kind: the daemon going away
// mid-run, an image that doesn't exist, and the like. A container that
// could not be stopped at the deadline is not one of them: that stays a
// TimedOut CheckResult, with docker's own refusal reported on stderr and
// kept in Output. A timeout that fires before Docker has even created
// the container waits for it rather than letting the check command start
// behind the kill (issue #66), so such a run can return later than its
// own timeout - bounded, never indefinitely - and what stopping it took
// is reported on stderr and in Output the same way.
func RunCheck(repoRoot, command, image string, limits container.Limits, timeout time.Duration, stdout, stderr io.Writer) (CheckResult, error) {
	// os/exec copies stdout and stderr on separate goroutines unless the
	// two writers are the identical value, and tee never is, so the shared
	// capture buffer and the caller's writers all have to be safe for
	// concurrent writes.
	capture := newSyncWriter(OutputHeadBytes, OutputTailBytes)

	// Resolved before anything starts, so a repo whose git directory
	// cannot be reached inside the container is refused rather than
	// handed a check command whose every git call would fail. The error
	// wraps container.ErrGitDirUnavailable; Run turns that into a
	// refusal naming it.
	gitMounts, err := container.ResolveGitMounts(repoRoot)
	if err != nil {
		return CheckResult{}, err
	}
	defer gitMounts.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Resolved before anything starts, for the same reason as
	// gitMounts: a host user the container cannot actually be run as
	// (e.g. a remote Docker endpoint) is refused rather than discovered
	// mid-run. The error wraps container.ErrRemoteDocker; Run turns
	// that into a refusal naming it.
	user, err := container.ResolveUser(ctx)
	if err != nil {
		return CheckResult{}, err
	}

	// Resolved before anything starts, for the same reason again: a
	// repo whose source inspector cannot mount read-only is refused
	// rather than checked with that source writable. Both errors wrap
	// container.ErrWorkspaceUnavailable; Run turns that into a refusal
	// naming it.
	sourceEntries, err := TopLevelSourceEntries(repoRoot)
	if err != nil {
		return CheckResult{}, fmt.Errorf("%w: inspector cannot tell which of this repository's top-level paths hold tracked source, and those are exactly what it mounts read-only: %v", container.ErrWorkspaceUnavailable, err)
	}
	workspace, err := container.ResolveWorkspace(repoRoot, sourceEntries)
	if err != nil {
		return CheckResult{}, err
	}
	// Not deferred unconditionally: a run that reached a result hands
	// its workspace to the caller, whose report is written before it is
	// discarded. Every path that returns no result discards it here.
	keepWorkspace := false
	defer func() {
		if !keepWorkspace {
			workspace.Cleanup()
		}
	}()

	run := container.New(ctx, container.Run{
		WorkspaceRoot:  workspace.Root,
		SourceMounts:   workspace.SourceMounts,
		WritableMounts: workspace.WritableMounts,
		Command:        command,
		Image:          image,
		Limits:         limits,
		User:           user,
		ReadOnlyMounts: gitMounts.Mounts,
	})
	defer run.Cleanup()
	run.Stdout = capture.tee(stdout)
	run.Stderr = capture.tee(stderr)

	// cmd.Stdout/cmd.Stderr are io.Writer values, not *os.File, so
	// os/exec owns the pipes itself: it copies through its own
	// goroutines and cmd.Run's Wait blocks until those goroutines drain
	// to EOF before returning. Using cmd.StdoutPipe/StderrPipe instead
	// would hand that draining to the caller, and calling Wait before
	// the caller has read to EOF is the classic way to truncate output,
	// since Wait closes the read end once the process exits.
	err = run.Run()
	if err != nil && !run.Started() {
		// docker never got as far as creating the container - a bad
		// image, an unreachable registry, the daemon going away mid-
		// call. Whatever exit code the docker CLI produced belongs to
		// docker, not to command, so this cannot become a CheckResult:
		// doing so would risk reading it as the check's own verdict.
		return CheckResult{}, fmt.Errorf("starting the container: %w", err)
	}

	// What stopping this run took, when it took more than killing a
	// container that was there and running: a deadline that fired while
	// Docker was still creating the container was still enforced, but
	// only because the kill path waited for that container to exist
	// (issue #66), and a run where even waiting produced none had
	// Docker's own client killed instead. Neither is a failure, and both
	// are facts about this run that whoever reads its report is entitled
	// to - so they go in the report as well as live on stderr.
	if note := run.StopNote(); note != "" {
		fmt.Fprintf(run.Stderr, "\ninspector: %s\n", note)
	}

	// A container inspector tried and failed to stop is the one outcome
	// that must never be silent, and run.Run's error can't be trusted to
	// carry it either way: exec.Cmd.Wait drops Cancel's error whenever
	// the `docker run` client itself exited badly - exactly the case
	// where the container was left running - and surfaces it as a bare
	// Go error in the harmless race where the container had already
	// finished, which would cost this run both its timeout verdict and
	// its saved report. container.Cmd records the kill's own outcome
	// instead, so read it from there and put it in front of the person
	// running the check, in the report as well as live on stderr.
	killErr := run.KillError()
	if killErr != nil {
		fmt.Fprintf(run.Stderr, "\ninspector: could not stop the check container: %v\n"+
			"inspector: it may still be running against this repo - check `docker ps` and stop it by hand\n", killErr)
	}

	// Read once, after the kill notice above has gone through the same
	// writer: the capture is bounded, so a later read could report a
	// different dropped count than the one already written into it.
	output, dropped, printed := capture.captured()

	result, resultErr := classifyResult(run.Cmd, err, output)
	if resultErr != nil {
		if killErr == nil {
			return CheckResult{}, resultErr
		}
		// The only way here is Wait reporting the failed cancellation
		// itself, which says nothing about the check command. Cancel
		// runs only once the deadline has fired, so this is a timeout,
		// and the block below is what states that.
		result = CheckResult{ExitCode: -1, Output: output}
	}
	result.OutputDroppedBytes = dropped
	result.OutputTotalBytes = printed

	// Said live as well as in the report, since the person watching is
	// the one who still has the full output in front of them and can
	// keep it if they need it. Written to the caller's own writer rather
	// than through the tee, which would feed the capture it is
	// describing and make its own numbers stale.
	if dropped > 0 && stderr != nil {
		fmt.Fprintf(stderr, "\ninspector: this check command printed %d bytes; its saved report keeps the first %d and the last %d, and says so.\n",
			printed, OutputHeadBytes, OutputTailBytes)
	}

	// The Go documentation for CommandContext recommends judging a
	// timeout from ctx.Err() rather than from the shape of the returned
	// error, since Cancel firing doesn't guarantee any particular error
	// value once the process is reaped - including a race where the
	// container happens to exit cleanly on its own right as the deadline
	// fires, before the kill lands. Applied last and unconditionally, so
	// that race can't leave a timed-out run looking like ExitCode 0.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.TimedOut = true
		result.ExitCode = -1
		if result.Signal == "" {
			result.Signal = "killed"
		}
	}
	result.KillFailed = killErr != nil
	result.Workspace = workspace
	keepWorkspace = true
	return result, nil
}

// classifyResult turns cmd.Run's return into a CheckResult, given the
// output already captured. The returned error is reserved for genuine
// infrastructure failures - the caller's own writer failed - as opposed
// to the check command's own exit status, which never produces a Go
// error here.
func classifyResult(cmd *exec.Cmd, err error, output string) (CheckResult, error) {
	if err == nil {
		return CheckResult{ExitCode: 0, Output: output}, nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return CheckResult{
			ExitCode: exitErr.ExitCode(),
			Signal:   classifySignal(exitErr.ProcessState),
			Output:   output,
		}, nil
	}

	// cmd.Run can fail without an *exec.ExitError in two situations,
	// both still leaving cmd.ProcessState populated correctly from the
	// local `docker run` client's own completed wait4:
	//
	//  - WaitDelay elapsed waiting for the docker CLI's own stdout/
	//    stderr to close after it exited (e.g. a hung daemon connection
	//    that left a pipe open) - the returned error wraps
	//    exec.ErrWaitDelay. Containerizing removed the previous, more
	//    common cause (a check command leaking a background process
	//    that outlived it while still holding the pipe): the kernel
	//    tears down a container's whole PID namespace the moment its
	//    init process exits, so nothing inside it can outlive that exit
	//    the way a host subprocess could. This branch is kept as
	//    defensive handling for the docker-CLI-level case, not because
	//    the container case is expected to hit it in practice.
	//  - The container exited (naturally, or because container.Cmd's own
	//    kill lost the race and found it already gone) right as
	//    RunCheck's deadline fired. killContainer maps that "already
	//    gone" result to os.ErrProcessDone, which is exec.Cmd.Cancel's
	//    documented way of saying Wait should keep reflecting the
	//    process's own real exit instead of treating the race as a
	//    Cancel failure - but if the local `docker run` process was
	//    still finishing its own exit bookkeeping at that exact instant,
	//    Wait can instead adopt the context's own error, surfacing as a
	//    bare context.DeadlineExceeded.
	//
	// Gated specifically to these two errors, not any non-ExitError: a
	// caller's own writer failing (e.g. stdout redirected to a full
	// disk) reaches this branch too, with ProcessState populated and a
	// misleadingly clean exit code, but surfaces as neither of these -
	// so it correctly falls through instead of being folded into a
	// false green.
	if (errors.Is(err, exec.ErrWaitDelay) || errors.Is(err, context.DeadlineExceeded)) && cmd.ProcessState != nil {
		return CheckResult{
			ExitCode: cmd.ProcessState.ExitCode(),
			Signal:   classifySignal(cmd.ProcessState),
			Output:   output,
		}, nil
	}

	return CheckResult{}, err
}

// classifySignal reports what killed the process behind ps, checking
// both of the ways a POSIX wait status can say so. A process signaled
// directly reports it through WaitStatus.Signaled() - which is how the
// local `docker run` client itself would show up, in the rare case Go's
// own WaitDelay fallback force-killed it directly. But `sh -c "a && b"`
// forks a child per command and waits on it, so a compound command's own
// killed child doesn't signal sh itself - sh instead exits normally with
// 128+N, the shell's own convention for "my child died from signal N,"
// which is what killing a timed-out compound check command, an OOM-
// killed one, or a container.Cmd.Cancel-killed one, actually looks like
// from here: the check command still runs as `sh -c command` (now inside
// the container rather than on the host), so the same convention
// applies unchanged. Returns "" when neither applies.
func classifySignal(ps *os.ProcessState) string {
	if status, ok := ps.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return status.Signal().String()
	}
	if code := ps.ExitCode(); code >= 128 {
		return syscall.Signal(code - 128).String()
	}
	return ""
}

// syncWriter keeps the first headLimit bytes written to it and the last
// tailLimit, counting what fell between them, and never holds more than
// that however much is written - a check command that prints tens of
// megabytes costs one run headLimit+tailLimit of memory rather than
// several multiples of its own output (issue #12). The tail is a fixed
// ring: once full, each further byte displaces the oldest one it holds.
type syncWriter struct {
	mu sync.Mutex

	headLimit int
	tailLimit int

	head []byte
	// tail grows to tailLimit and then stops; from that point tailPos is
	// where the next byte goes and also where the oldest kept byte lives.
	tail     []byte
	tailPos  int
	tailFull bool

	dropped int64
	printed int64
}

func newSyncWriter(headLimit, tailLimit int) *syncWriter {
	return &syncWriter{
		headLimit: headLimit,
		tailLimit: tailLimit,
		head:      make([]byte, 0, headLimit),
		tail:      make([]byte, 0, tailLimit),
	}
}

// captured reports what was kept, how much was dropped to keep it, and
// how much was written in total. When anything was dropped, output
// carries one marker line in the gap saying so, so a report of it can
// never be read as the whole output.
func (w *syncWriter) captured() (output string, dropped, printed int64) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// tailPos means nothing until the ring is full; until then the tail
	// is simply everything written after the head, in order.
	tail := string(w.tail)
	if w.tailFull {
		tail = string(w.tail[w.tailPos:]) + string(w.tail[:w.tailPos])
	}
	if w.dropped == 0 {
		return string(w.head) + tail, 0, w.printed
	}
	// ToValidUTF8 only on a truncated capture, and only because both
	// cuts can land mid-rune; an untruncated one is handed back exactly
	// as the command printed it.
	marker := fmt.Sprintf(
		"\n[inspector: %d bytes of this check command's output were dropped here. It printed %d bytes in total; this report keeps the first %d and the last %d. The full output was streamed live as the check ran.]\n",
		w.dropped, w.printed, len(w.head), len(tail),
	)
	return strings.ToValidUTF8(string(w.head), "") + marker + strings.ToValidUTF8(tail, ""), w.dropped, w.printed
}

// keep records p, filling the head first and then the tail ring. The
// caller holds w.mu.
func (w *syncWriter) keep(p []byte) {
	w.printed += int64(len(p))

	if room := w.headLimit - len(w.head); room > 0 {
		n := min(room, len(p))
		w.head = append(w.head, p[:n]...)
		p = p[n:]
	}
	if len(p) == 0 {
		return
	}

	if !w.tailFull {
		n := min(w.tailLimit-len(w.tail), len(p))
		w.tail = append(w.tail, p[:n]...)
		p = p[n:]
		if len(w.tail) == w.tailLimit {
			w.tailFull = true
			w.tailPos = 0
		}
		if len(p) == 0 {
			return
		}
	}

	// The ring is full, so every remaining byte costs an older one.
	if len(p) >= w.tailLimit {
		w.dropped += int64(w.tailLimit) + int64(len(p)-w.tailLimit)
		copy(w.tail, p[len(p)-w.tailLimit:])
		w.tailPos = 0
		return
	}
	w.dropped += int64(len(p))
	if n := copy(w.tail[w.tailPos:], p); n < len(p) {
		copy(w.tail, p[n:])
	}
	w.tailPos = (w.tailPos + len(p)) % w.tailLimit
}

// tee returns a writer that copies to dst and to w, holding w's lock for
// both, so concurrent stdout and stderr copies never write to dst at the
// same time.
func (w *syncWriter) tee(dst io.Writer) io.Writer {
	return &teeWriter{capture: w, dst: dst}
}

type teeWriter struct {
	capture *syncWriter
	dst     io.Writer
}

func (t *teeWriter) Write(p []byte) (int, error) {
	t.capture.mu.Lock()
	defer t.capture.mu.Unlock()

	n, err := t.dst.Write(p)
	if err != nil {
		return n, err
	}
	if n != len(p) {
		return n, io.ErrShortWrite
	}
	t.capture.keep(p)
	return len(p), nil
}
