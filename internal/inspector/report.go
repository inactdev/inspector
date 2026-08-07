package inspector

import (
	"encoding/json"
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

	name := fmt.Sprintf("%d-%s.json", r.FinishedAt.Unix(), shortSHA(r.Commit))
	path := filepath.Join(runsDir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}

	latest := filepath.Join(repoRoot, RunsDirName, LatestReportName)
	if err := os.WriteFile(latest, data, 0o644); err != nil {
		return "", fmt.Errorf("writing %s: %w", latest, err)
	}

	return path, nil
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
