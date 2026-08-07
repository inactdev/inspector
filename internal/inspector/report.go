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
// notes for a human chasing down a red result, never authority. The
// commit status (issue #3) is the record that gates anything.
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
	Output       string    `json:"output"`
}

// WriteReport persists r under repoRoot/RunsDirName/runs/ and refreshes
// LatestReportName. It returns the path of the timestamped report file.
func WriteReport(repoRoot string, r Report) (string, error) {
	runsDir := filepath.Join(repoRoot, RunsDirName, "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", runsDir, err)
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

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
