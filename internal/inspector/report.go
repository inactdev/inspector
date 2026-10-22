package inspector

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// RunsDirName holds one JSON report per run. It is gitignored - these are
// notes for a human chasing down a red result, never authority. inspector-gate
// reads only the green commit status, not these reports.
const RunsDirName = ".inspector"

// LatestReportName is a copy of the most recent report, so a red run is
// actionable without hunting through RunsDirName for the newest file.
const LatestReportName = "latest.json"

// Report is the local record of one inspector run.
type Report struct {
	Commit       string    `json:"commit"`
	Repo         string    `json:"repo"`
	Claim        string    `json:"claim,omitempty"`
	CheckCommand string    `json:"checkCommand"`
	Outcome      Outcome   `json:"outcome"`
	StartedAt    time.Time `json:"startedAt"`
	FinishedAt   time.Time `json:"finishedAt"`
	DurationMS   int64     `json:"durationMs"`
	ExitCode     int       `json:"exitCode"`
	Signal       string    `json:"signal,omitempty"`
	TimedOut     bool      `json:"timedOut,omitempty"`
	Output       string    `json:"output"`
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

// WriteReport persists r under repoRoot/RunsDirName/runs/ and refreshes
// LatestReportName. It returns the path of the timestamped report file.
//
// r.Output is whatever RunCheck captured, unbounded, and is written out in
// full here twice; RunsDirName also never prunes old runs. Deliberately
// deferred - see issue #12.
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
