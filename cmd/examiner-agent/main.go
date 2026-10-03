//go:build examiner_agent

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/inactdev/inspector/internal/examiner"
)

const (
	exitGreen      = 0
	exitRefused    = 2
	exitUsage      = 64
	defaultTimeout = 10 * time.Minute
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("examiner-agent", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	inputDir := fs.String("input-dir", "", "sealed input directory")
	outputDir := fs.String("output-dir", "", "sealed output directory")
	appURL := fs.String("app-url", "", "running app URL")
	model := fs.String("model", "", "Anthropic model")
	apiBaseURL := fs.String("api-base-url", "", "Anthropic Messages API URL")
	timeout := fs.Duration("timeout", defaultTimeout, "maximum examination duration")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	for _, required := range []struct{ name, value string }{
		{"--input-dir", *inputDir}, {"--output-dir", *outputDir}, {"--app-url", *appURL}, {"--model", *model},
	} {
		if strings.TrimSpace(required.value) == "" {
			fmt.Fprintf(os.Stderr, "examiner-agent: missing required %s\n", required.name)
			return exitUsage
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if err := examiner.RunAgent(ctx, examiner.AgentOptions{
		InputDir: *inputDir, OutputDir: *outputDir, AppURL: *appURL, Model: *model, APIBaseURL: *apiBaseURL,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "examiner-agent: %v\n", err)
		return exitRefused
	}
	return exitGreen
}
