package inspector

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// RunsDirName holds one JSON report per run. It is gitignored - these are
// notes for a human chasing down a red result, never authority. inspector-gate
// reads only the green commit status, not these reports.
const RunsDirName = ".inspector"

// LatestReportName is a copy of the most recent report, so a red run is
// actionable without hunting through RunsDirName for the newest file.
// Deliberately still a second full copy of the same report: it is the file
// Fabrica copies onto the task record and the one a green status's own
// description points a reader at, so both writes have readers (issue #12).
const LatestReportName = "latest.json"

// MaxRunsKept is how many run reports RunsDirName/runs keeps. Older ones
// are deleted as each new report is written, so a repo checked many times
// a day stops growing without limit. Ruled 2026-10-28 for issue #12 and
// fixed on purpose - no config key and no flag - and paired with
// MaxOutputBytes, which is what makes a count a real bound: 20 runs can
// no longer mean 20 files of unbounded size.
//
// Pruning happens after the new report is written and never removes that
// report, so the run whose status is about to be posted always keeps its
// notes - issue #12's one hard constraint on this. Older runs are fair
// game by the same document that makes the constraint cheap: a report is
// notes, never authority (SPEC.md section 12), and the status itself
// lives on the commit at GitHub, where nothing here can reach it.
const MaxRunsKept = 20

// ErrRunsNotPruned reports that this run's report was written but older
// ones could not be removed. It is deliberately distinguishable from a
// failed save: the newest record is worth more than the bound on the
// directory, so Run turns this into a loud warning on a result that
// stands, not into "the report could not be saved."
var ErrRunsNotPruned = errors.New("older run reports could not be removed")

// Report is the local record of one inspector run.
type Report struct {
	Commit       string  `json:"commit"`
	Repo         string  `json:"repo"`
	Claim        string  `json:"claim,omitempty"`
	CheckCommand string  `json:"checkCommand"`
	Outcome      Outcome `json:"outcome"`
	// TermsDir names the folder this run read the project's terms from -
	// its .inspector.json, its Dockerfile.inspector and the check script
	// they name - and is absent on the ordinary run that read them from
	// the repository root, where the commit above already records them
	// (issue #69). Present, it is the only record of what judged this
	// commit, since those files are in no commit at all.
	TermsDir string `json:"termsDir,omitempty"`
	// NoStamp records that this run was told to publish without posting
	// any commit status. Absent on every ordinary run: the stamp is the
	// default, and a run that skipped it on purpose must not read like
	// one whose stamp was never confirmed.
	NoStamp bool `json:"noStamp,omitempty"`
	// Image is the local tag of the container image this check ran in,
	// built from the project's own ProjectDockerfileName, and ImageBases
	// names each base that file pulled in with the image ID docker
	// resolved it to. Together they are what the tag is derived from, and
	// the only record of which environment judged this commit.
	Image      string            `json:"image,omitempty"`
	ImageBases map[string]string `json:"imageBases,omitempty"`
	StartedAt  time.Time         `json:"startedAt"`
	FinishedAt time.Time         `json:"finishedAt"`
	DurationMS int64             `json:"durationMs"`
	ExitCode   int               `json:"exitCode"`
	Signal     string            `json:"signal,omitempty"`
	TimedOut   bool              `json:"timedOut,omitempty"`
	// Output is what the check command printed, bounded to
	// MaxOutputBytes: its first OutputHeadBytes and last
	// OutputTailBytes, with one marker line between them naming the
	// dropped middle. OutputTruncated says that happened without a
	// reader having to find that line, OutputDroppedBytes is how much
	// Output does not hold, and OutputTotalBytes is what the command
	// printed altogether. All three are absent on the ordinary run that
	// printed less than the cap.
	Output             string `json:"output"`
	OutputTruncated    bool   `json:"outputTruncated,omitempty"`
	OutputDroppedBytes int64  `json:"outputDroppedBytes,omitempty"`
	OutputTotalBytes   int64  `json:"outputTotalBytes,omitempty"`
	// PullRequestURL is set after the fact, once a green run has opened
	// or found the pull request for its published branch - WriteReport
	// runs before that step even exists, so RecordPullRequestURL is what
	// patches it into both this file and LatestReportName afterward.
	PullRequestURL string `json:"pullRequestURL,omitempty"`
	// TestReview is the evidence behind this run's TestsStatusContext
	// status. Like PullRequestURL it is patched in afterward, by
	// RecordTestReview - the judgment it records cannot happen until after
	// the check this report is written for. Absent on a run that judged no
	// test change at all.
	TestReview *TestReview `json:"testReview,omitempty"`
}

// TestReview is what the test-reviewer concluded about one run's changed test
// files, and who judged them. The TestsStatusContext status carries a single
// description GitHub caps at 140 characters, so the per-file reasons can only
// ever be read here: the status is the signal, this is the evidence.
//
// It is recorded for a pass exactly as for a failure, because a pass that
// records no reasoning is unauditable - a model that waves every diff through
// looks identical to a careful one (issue #55).
type TestReview struct {
	// Vendor and Model name who judged, the same two values the status
	// description names. Model is empty when none was configured, which is
	// itself one of the failing states.
	Vendor string `json:"vendor,omitempty"`
	Model  string `json:"model,omitempty"`
	// State is the status state this review produced, and Verdict is
	// inspector's own derived answer behind it. Verdict is empty when the
	// review failed before it could read the change at all.
	State   StatusState `json:"state"`
	Verdict string      `json:"verdict,omitempty"`
	// Summary is the one line the run also printed - for a failure, the
	// reason it failed.
	Summary string `json:"summary"`
	// Files is every changed test file the review covered, in diff order. A
	// file the reviewer never judged carries an empty Judgment rather than
	// being left out: "nobody judged this" is a fact about the review, not
	// an absence.
	Files []TestReviewFile `json:"files,omitempty"`
}

// TestReviewFile is one changed test file as the review saw it: what the
// change did to it, and the judgment and reason the reviewer gave for it.
type TestReviewFile struct {
	Path     string `json:"path"`
	Status   string `json:"status"`
	Judgment string `json:"judgment,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// WriteReport persists r under repoRoot/RunsDirName/runs/, refreshes
// LatestReportName, and then removes all but the newest MaxRunsKept run
// reports. It returns the path of the timestamped report file.
//
// r.Output is already bounded to MaxOutputBytes by the time it gets here
// (see RunCheck), so each of the two files it is written to is bounded
// too, and the directory holding them has a stated ceiling rather than
// growing forever (issue #12). A report that was written but whose older
// siblings could not be removed returns its path together with an error
// wrapping ErrRunsNotPruned, which is not a failed save.
func WriteReport(repoRoot string, r Report) (string, error) {
	runsDir := filepath.Join(repoRoot, RunsDirName, "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", runsDir, err)
	}
	if err := ensureIgnored(filepath.Join(repoRoot, RunsDirName)); err != nil {
		return "", err
	}

	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encoding report: %w", err)
	}
	data = append(data, '\n')

	path, err := writeNewFile(runsDir, fmt.Sprintf("%d-%s", r.FinishedAt.Unix(), shortSHA(r.Commit)), data)
	if err != nil {
		return "", err
	}

	latest := filepath.Join(repoRoot, RunsDirName, LatestReportName)
	if err := os.WriteFile(latest, data, 0o644); err != nil {
		return "", fmt.Errorf("writing %s: %w", latest, err)
	}

	// Last, and with its own error: this run's own two files are what
	// matter, and neither of them should be lost over an old one that
	// would not delete.
	if err := pruneRuns(runsDir, MaxRunsKept, filepath.Base(path)); err != nil {
		return path, err
	}

	return path, nil
}

// pruneRuns leaves the newest keep run reports in runsDir and removes the
// rest, never removing the report named by protect - the one just
// written, whose status is the one about to be posted. That exclusion is
// structural rather than inferred from the ordering below, because issue
// #12's one hard constraint is that pruning must not be *able* to remove
// the run a commit status refers to.
//
// It only ever removes files inspector itself wrote - the
// <unix-seconds>-<short-sha>[-n].json names writeNewFile produces -
// because a gitignored directory is also somewhere a person may have put
// something of their own, and deleting that would be inspector editing
// what it was not asked to.
func pruneRuns(runsDir string, keep int, protect string) error {
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return fmt.Errorf("%w: reading %s: %v", ErrRunsNotPruned, runsDir, err)
	}
	var reports []runReportFile
	for _, entry := range entries {
		if entry.IsDir() || !isRunReportName(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			// Gone between the listing and the stat, so there is
			// nothing left to prune and nothing to complain about.
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("%w: reading %s: %v", ErrRunsNotPruned, filepath.Join(runsDir, entry.Name()), err)
		}
		reports = append(reports, runReportFile{name: entry.Name(), written: info.ModTime()})
	}
	if len(reports) <= keep {
		return nil
	}

	sort.Slice(reports, func(i, j int) bool { return reports[i].newerThan(reports[j]) })
	var failed []string
	for _, report := range reports[keep:] {
		if report.name == protect {
			continue
		}
		// A concurrent run pruning the same directory can get here
		// first; the file being gone already is the outcome asked for,
		// not a failure.
		if err := os.Remove(filepath.Join(runsDir, report.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			failed = append(failed, err.Error())
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%w: %s", ErrRunsNotPruned, strings.Join(failed, "; "))
	}
	return nil
}

// runReportFile is one report in RunsDirName/runs, with when it was
// written.
type runReportFile struct {
	name    string
	written time.Time
}

// newerThan orders reports newest first. Modification time decides it,
// not the name: pruning frees a name for writeNewFile to reuse, so two
// runs in one second can end up with the earlier-written one carrying the
// later-looking suffix. The name is only the tie-break, for a file system
// whose timestamps are too coarse to separate two runs of the same
// second - the leading unix seconds first, then writeNewFile's own
// collision suffixes, which grow: an unsuffixed name came before "-1",
// and a longer suffix after a shorter one ("-10" after "-2").
func (r runReportFile) newerThan(other runReportFile) bool {
	if !r.written.Equal(other.written) {
		return r.written.After(other.written)
	}
	if sa, sb := runReportStamp(r.name), runReportStamp(other.name); sa != sb {
		return sa > sb
	}
	if len(r.name) != len(other.name) {
		return len(r.name) > len(other.name)
	}
	return r.name > other.name
}

// isRunReportName reports whether name is one writeNewFile could have
// produced: unix seconds, a dash, and something ending in .json. It
// deliberately does not match LatestReportName or any other hand-written
// file.
func isRunReportName(name string) bool {
	rest, ok := strings.CutSuffix(name, ".json")
	if !ok {
		return false
	}
	stamp, rest, ok := strings.Cut(rest, "-")
	if !ok || stamp == "" || rest == "" {
		return false
	}
	_, err := strconv.ParseInt(stamp, 10, 64)
	return err == nil
}

func runReportStamp(name string) int64 {
	stamp, _, _ := strings.Cut(strings.TrimSuffix(name, ".json"), "-")
	seconds, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		return 0
	}
	return seconds
}

// ImageBuildLogName holds the output of the most recent check-image
// build that failed, under RunsDirName with every other note this run
// leaves behind. Overwritten rather than accumulated, like
// LatestReportName: a build that failed is acted on now, and the one
// before it has already been superseded by the attempt that replaced it.
const ImageBuildLogName = "image-build.log"

// WriteImageBuildLog keeps docker's own output from a failed check-image
// build, which is the only thing that says why it failed, and returns the
// path it was written to. Notes only, never authority - the refusal
// itself is the result; this is what a human reads to fix it.
func WriteImageBuildLog(repoRoot, dockerfile, log string) (string, error) {
	dir := filepath.Join(repoRoot, RunsDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}
	if err := ensureIgnored(dir); err != nil {
		return "", err
	}
	path := filepath.Join(dir, ImageBuildLogName)
	header := fmt.Sprintf("# docker build of %s failed; its output follows.\n", dockerfile)
	if err := os.WriteFile(path, []byte(header+log), 0o644); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	return path, nil
}

// ensureIgnored makes dir ignore its own contents, so reports - which
// carry the check command's full output - never end up committed to the
// repo being inspected. It is written once, on the first run, and an
// existing file is left exactly as the user wrote it.
func ensureIgnored(dir string) error {
	path := filepath.Join(dir, ".gitignore")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if _, err := f.WriteString("*\n"); err != nil {
		f.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// writeNewFile writes data to dir/base.json without ever overwriting an
// existing report - two runs against the same commit in the same second
// would otherwise collide and silently lose one run's record.
func writeNewFile(dir, base string, data []byte) (string, error) {
	for n := 0; ; n++ {
		name := base + ".json"
		if n > 0 {
			name = fmt.Sprintf("%s-%d.json", base, n)
		}
		path := filepath.Join(dir, name)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("writing %s: %w", path, err)
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			return "", fmt.Errorf("writing %s: %w", path, err)
		}
		if err := f.Close(); err != nil {
			return "", fmt.Errorf("writing %s: %w", path, err)
		}
		return path, nil
	}
}

// RecordPullRequestURL patches a just-opened (or just-found) pull request's
// URL into the report already written for this run at reportPath, and into
// LatestReportName alongside it, so a reader of either file sees it without
// re-running inspector. A no-op when reportPath is empty - the report save
// itself already failed, and the local report is notes, never authority.
func RecordPullRequestURL(repoRoot, reportPath, url string) error {
	return patchReport(repoRoot, reportPath, func(r *Report) { r.PullRequestURL = url })
}

// RecordTestReview patches the changed-test review into the report already
// written for this run at reportPath, and into LatestReportName alongside it.
// The judgment happens after the check, so WriteReport cannot carry it; the
// same no-op rule as RecordPullRequestURL applies when the report save itself
// already failed.
func RecordTestReview(repoRoot, reportPath string, review TestReview) error {
	return patchReport(repoRoot, reportPath, func(r *Report) { r.TestReview = &review })
}

// patchReport applies mutate to this run's report and to LatestReportName,
// which holds a copy of it. A no-op when reportPath is empty - the report
// save itself already failed, and the local report is notes, never authority.
func patchReport(repoRoot, reportPath string, mutate func(*Report)) error {
	if reportPath == "" {
		return nil
	}
	if err := patchReportFile(reportPath, mutate); err != nil {
		return err
	}
	return patchReportFile(filepath.Join(repoRoot, RunsDirName, LatestReportName), mutate)
}

func patchReportFile(path string, mutate func(*Report)) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	var report Report
	if err := json.Unmarshal(data, &report); err != nil {
		return fmt.Errorf("decoding %s: %w", path, err)
	}
	mutate(&report)
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding %s: %w", path, err)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
