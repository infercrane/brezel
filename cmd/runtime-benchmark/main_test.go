package main

import (
	"errors"
	"testing"
	"time"

	"github.com/infercrane/sandbox-runtime-lab/internal/conformance"
)

func TestSummarizeUsesNearestRankPercentiles(t *testing.T) {
	got := summarize([]int64{100, 5, 7, 8, 10})
	if got.Samples != 5 || got.MinMS != 5 || got.P50MS != 8 || got.P95MS != 100 || got.P99MS != 100 || got.MaxMS != 100 || got.MeanMS != 26 {
		t.Fatalf("summarize() = %#v", got)
	}
}

func TestAggregateExcludesIncompleteRunsFromLatency(t *testing.T) {
	now := time.Now().UTC()
	runStarted := now.Add(-2 * time.Second)
	results := []runResult{
		{index: 0, report: conformance.Report{Qualification: "sandbox_runtime_conformant", StartedAt: runStarted, FinishedAt: now, Steps: []conformance.Step{{Name: "create_sandbox", Status: "passed", DurationMS: 12}}}},
		{index: 1, report: conformance.Report{Qualification: "failed", Steps: []conformance.Step{{Name: "create_sandbox", Status: "passed", DurationMS: 999}}}, err: errors.New("later step failed")},
	}
	report := aggregate("host", 3, 1, runStarted, now, results)
	if report.Succeeded != 1 || report.Failed != 2 || len(report.Failures) != 1 {
		t.Fatalf("aggregate counts = %#v", report)
	}
	if report.SchemaVersion != 2 || report.WallDurationMS != 2000 || report.ThroughputRunsPerMinute != 30 {
		t.Fatalf("aggregate rate = %#v", report)
	}
	if got := report.Measurements["create_sandbox"]; got.Samples != 1 || got.P50MS != 12 {
		t.Fatalf("measurement = %#v", got)
	}
	if got := report.Measurements["complete_run"]; got.Samples != 1 || got.P50MS != 2000 {
		t.Fatalf("complete run measurement = %#v", got)
	}
}
