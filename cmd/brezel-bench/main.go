package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/infercrane/brezel/internal/perfbench"
	"github.com/infercrane/brezel/internal/securefile"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("brezel-bench", flag.ContinueOnError)
	flags.SetOutput(stderr)
	baseURL := flags.String("base-url", "http://127.0.0.1:8080", "runtime control API URL")
	project := flags.String("project", "brezel-benchmark", "isolated project used for benchmark resources")
	template := flags.String("backend-template", "", "existing backend template ID")
	target := flags.String("target", "", "human-readable identity of the exact deployment")
	runtimeRevision := flags.String("runtime-revision", "", "runtime release or source revision under test")
	evidenceClass := flags.String("evidence-class", "", "synthetic-test, local-docker, single-host-linux-kvm, or hosted-end-to-end")
	cacheState := flags.String("cache-state", "", "cold, cached-template, warm-pool, or unknown")
	scenario := flags.String("scenario", "tti", "tti, warm-exec, resume, filesystem-checkpoint, filesystem-restore, preview-first-byte, preview-warm, or workspace-io")
	mode := flags.String("mode", "sequential", "sequential, staggered, or burst")
	runs := flags.Int("runs", 20, "number of measured attempts (1-256)")
	maxInFlight := flags.Int("max-in-flight", 0, "request concurrency; defaults to 1 for sequential and runs otherwise")
	stagger := flags.Duration("stagger", 200*time.Millisecond, "delay between staggered arrivals")
	ioBytes := flags.Int("io-bytes", 1<<20, "generated workspace payload bytes for workspace-io (1-33554432)")
	previewPort := flags.Uint("preview-port", 8080, "guest HTTP port for preview scenarios (1-65535)")
	attemptTimeout := flags.Duration("attempt-timeout", 2*time.Minute, "timeout for one setup or measured operation")
	cleanupTimeout := flags.Duration("cleanup-timeout", 2*time.Minute, "per-resource cleanup timeout")
	wholeTimeout := flags.Duration("timeout", 45*time.Minute, "whole benchmark timeout")
	execute := flags.Bool("execute", false, "create and delete real runtime resources")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("brezel-bench does not accept positional arguments")
	}
	if !*execute {
		return errors.New("benchmark requires explicit -execute because it creates and deletes real runtime resources")
	}
	if *maxInFlight == 0 {
		*maxInFlight = 1
		if *mode != string(perfbench.ModeSequential) {
			*maxInFlight = *runs
		}
	}
	if *previewPort == 0 || *previewPort > 65535 {
		return errors.New("preview-port must be between 1 and 65535")
	}
	if *ioBytes < 1 || *ioBytes > 32<<20 {
		return errors.New("io-bytes must be between 1 and 33554432")
	}
	token, err := loadServiceToken()
	if err != nil {
		return err
	}
	config := perfbench.Config{
		BaseURL:         *baseURL,
		Token:           token,
		ProjectID:       *project,
		BackendTemplate: *template,
		Target:          *target,
		RuntimeRevision: *runtimeRevision,
		EvidenceClass:   perfbench.EvidenceClass(*evidenceClass),
		CacheState:      perfbench.CacheState(*cacheState),
		Scenario:        perfbench.Scenario(*scenario),
		Mode:            perfbench.Mode(*mode),
		Runs:            *runs,
		MaxInFlight:     *maxInFlight,
		StaggerInterval: *stagger,
		AttemptTimeout:  *attemptTimeout,
		CleanupTimeout:  *cleanupTimeout,
		IOBytes:         *ioBytes,
		PreviewPort:     uint16(*previewPort),
		Execute:         true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), *wholeTimeout)
	defer cancel()
	report, benchmarkErr := perfbench.Run(ctx, config)
	if report.SchemaVersion != 0 {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			return fmt.Errorf("encode benchmark report: %w", err)
		}
	}
	if benchmarkErr != nil {
		return benchmarkErr
	}
	return nil
}

func loadServiceToken() (string, error) {
	path := os.Getenv("BREZEL_SERVICE_TOKEN_FILE")
	if path == "" {
		return "", errors.New("BREZEL_SERVICE_TOKEN_FILE is required; the token is intentionally not accepted in argv or environment values")
	}
	token, err := securefile.ReadText(path, 16<<10)
	if err != nil {
		return "", fmt.Errorf("read runtime service token: %w", err)
	}
	if len(token) < 32 {
		return "", errors.New("token file does not contain a valid service token")
	}
	return token, nil
}
