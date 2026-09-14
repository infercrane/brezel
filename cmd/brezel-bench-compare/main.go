package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/infercrane/brezel/internal/perfgate"
)

type values []string

func (v *values) String() string { return strings.Join(*v, ",") }
func (v *values) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("value cannot be empty")
	}
	*v = append(*v, value)
	return nil
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("brezel-bench-compare", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var baseline, candidate, targetValues values
	flags.Var(&baseline, "baseline", "baseline summary.json; repeat at least twice")
	flags.Var(&candidate, "candidate", "candidate summary.json; repeat at least twice")
	flags.Var(&targetValues, "target", "required improvement as CASE:METRIC; repeat as needed")
	requiredImprovement := flags.Float64("required-improvement", 5, "minimum target improvement percent")
	maxLatencyRegression := flags.Float64("max-latency-regression", 5, "maximum non-target p95/p99 latency regression percent")
	maxThroughputLoss := flags.Float64("max-throughput-loss", 5, "maximum non-target concurrent throughput loss percent")
	absoluteTolerance := flags.Float64("absolute-latency-tolerance-ms", 2, "small-latency guard tolerance in milliseconds")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	targets := make([]perfgate.Target, 0, len(targetValues))
	for _, value := range targetValues {
		parts := strings.Split(value, ":")
		if len(parts) != 2 {
			return fmt.Errorf("invalid target %q; expected CASE:METRIC", value)
		}
		targets = append(targets, perfgate.Target{Case: parts[0], Metric: perfgate.Metric(parts[1])})
	}
	result, err := perfgate.Evaluate(perfgate.Config{
		BaselinePaths: baseline, CandidatePaths: candidate, Targets: targets,
		RequiredImprovement: *requiredImprovement, MaxLatencyRegression: *maxLatencyRegression,
		MaxThroughputLoss: *maxThroughputLoss, AbsoluteToleranceMS: *absoluteTolerance,
	})
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		return fmt.Errorf("encode comparison: %w", err)
	}
	if result.Decision != perfgate.DecisionPromote {
		return errors.New("benchmark promotion gate rejected the candidate")
	}
	return nil
}
