package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/infercrane/sandbox-runtime-lab/internal/conformance"
)

type measurement struct {
	Samples int     `json:"samples"`
	MinMS   int64   `json:"min_ms"`
	P50MS   int64   `json:"p50_ms"`
	P95MS   int64   `json:"p95_ms"`
	P99MS   int64   `json:"p99_ms"`
	MaxMS   int64   `json:"max_ms"`
	MeanMS  float64 `json:"mean_ms"`
}

type failure struct {
	Run   int    `json:"run"`
	Error string `json:"error"`
}

type benchmarkReport struct {
	SchemaVersion           int                    `json:"schema_version"`
	Target                  string                 `json:"target"`
	Scope                   string                 `json:"scope"`
	StartedAt               time.Time              `json:"started_at"`
	FinishedAt              time.Time              `json:"finished_at"`
	WallDurationMS          int64                  `json:"wall_duration_ms"`
	ThroughputRunsPerMinute float64                `json:"throughput_runs_per_minute"`
	Runs                    int                    `json:"runs"`
	Concurrency             int                    `json:"concurrency"`
	Succeeded               int                    `json:"succeeded"`
	Failed                  int                    `json:"failed"`
	Measurements            map[string]measurement `json:"measurements"`
	Failures                []failure              `json:"failures,omitempty"`
}

type runResult struct {
	index  int
	report conformance.Report
	err    error
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	baseURL := flag.String("base-url", "http://127.0.0.1:8080", "runtime control API URL")
	project := flag.String("project", "runtime-conformance", "project used for benchmark resources")
	otherProject := flag.String("other-project", "runtime-conformance-isolation", "project used for isolation checks")
	template := flag.String("backend-template", "base", "existing backend template ID")
	target := flag.String("target", "", "human-readable deployment identity")
	runs := flag.Int("runs", 20, "number of complete conformance runs")
	concurrency := flag.Int("concurrency", 1, "maximum concurrent conformance runs")
	timeout := flag.Duration("timeout", 30*time.Minute, "whole benchmark timeout")
	execute := flag.Bool("execute", false, "create and delete real backend resources")
	flag.Parse()

	if !*execute {
		return errors.New("benchmark requires explicit -execute because it creates backend resources")
	}
	if *target == "" {
		return errors.New("target is required")
	}
	if *runs < 1 || *runs > 1000 {
		return errors.New("runs must be between 1 and 1000")
	}
	if *concurrency < 1 || *concurrency > *runs || *concurrency > 64 {
		return errors.New("concurrency must be between 1 and min(runs, 64)")
	}
	token, err := readPrivateTokenFile(os.Getenv("RUNTIME_SERVICE_TOKEN_FILE"))
	if err != nil {
		return fmt.Errorf("read runtime service token: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	started := time.Now().UTC()
	jobs := make(chan int)
	results := make(chan runResult, *runs)
	var workers sync.WaitGroup
	for worker := 0; worker < *concurrency; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				runner, newErr := conformance.New(conformance.Config{
					BaseURL: *baseURL, Token: token, ProjectID: *project,
					OtherProjectID: *otherProject, BackendTemplate: *template,
					Target: fmt.Sprintf("%s-run-%04d", *target, index+1), Execute: true,
				})
				if newErr != nil {
					results <- runResult{index: index, err: newErr}
					continue
				}
				report, runErr := runner.Run(ctx)
				results <- runResult{index: index, report: report, err: runErr}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for index := 0; index < *runs; index++ {
			select {
			case jobs <- index:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		workers.Wait()
		close(results)
	}()

	completed := make([]runResult, 0, *runs)
	for result := range results {
		completed = append(completed, result)
	}
	sort.Slice(completed, func(i, j int) bool { return completed[i].index < completed[j].index })
	report := aggregate(*target, *runs, *concurrency, started, time.Now().UTC(), completed)
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return fmt.Errorf("benchmark incomplete: %w", ctx.Err())
	}
	if report.Failed != 0 || len(completed) != *runs {
		return fmt.Errorf("benchmark failed: %d of %d runs failed", report.Failed+(*runs-len(completed)), *runs)
	}
	return nil
}

func aggregate(target string, runs, concurrency int, started, finished time.Time, results []runResult) benchmarkReport {
	values := make(map[string][]int64)
	wallDuration := finished.Sub(started)
	output := benchmarkReport{
		SchemaVersion: 2, Target: target,
		Scope:     "operation-level wall-clock latency from complete destructive conformance runs against real backend resources; successful complete runs only",
		StartedAt: started, FinishedAt: finished, Runs: runs, Concurrency: concurrency,
		WallDurationMS: wallDuration.Milliseconds(),
		Measurements:   make(map[string]measurement),
	}
	for _, result := range results {
		if result.err != nil || result.report.Qualification != "sandbox_runtime_conformant" {
			message := "run did not reach sandbox_runtime_conformant"
			if result.err != nil {
				message = result.err.Error()
			}
			output.Failures = append(output.Failures, failure{Run: result.index + 1, Error: message})
			continue
		}
		output.Succeeded++
		if duration := result.report.FinishedAt.Sub(result.report.StartedAt).Milliseconds(); duration >= 0 {
			values["complete_run"] = append(values["complete_run"], duration)
		}
		for _, step := range result.report.Steps {
			if step.Status == "passed" {
				values[step.Name] = append(values[step.Name], step.DurationMS)
			}
		}
	}
	output.Failed = len(output.Failures) + runs - len(results)
	if wallDuration > 0 {
		output.ThroughputRunsPerMinute = math.Round((float64(output.Succeeded)/wallDuration.Minutes())*100) / 100
	}
	for name, samples := range values {
		output.Measurements[name] = summarize(samples)
	}
	return output
}

func summarize(input []int64) measurement {
	values := append([]int64(nil), input...)
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	var total int64
	for _, value := range values {
		total += value
	}
	return measurement{
		Samples: len(values), MinMS: values[0], P50MS: percentile(values, 0.50),
		P95MS: percentile(values, 0.95), P99MS: percentile(values, 0.99), MaxMS: values[len(values)-1],
		MeanMS: math.Round((float64(total)/float64(len(values)))*100) / 100,
	}
}

func percentile(sorted []int64, quantile float64) int64 {
	index := int(math.Ceil(quantile*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	return sorted[index]
}

func readPrivateTokenFile(path string) (string, error) {
	if path == "" {
		return "", errors.New("RUNTIME_SERVICE_TOKEN_FILE is required")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("token file must be a private regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (16<<10)+1))
	if err != nil {
		return "", err
	}
	if len(data) > 16<<10 {
		return "", errors.New("token file exceeds 16384 bytes")
	}
	token := strings.TrimSpace(string(data))
	if len(token) < 32 || strings.ContainsAny(token, "\r\n") {
		return "", errors.New("token file does not contain a valid service token")
	}
	return token, nil
}
