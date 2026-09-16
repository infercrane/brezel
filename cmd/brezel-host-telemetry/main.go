package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/infercrane/brezel/internal/hosttelemetry"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "brezel-host-telemetry:", err)
		os.Exit(1)
	}
}

func run() error {
	defaults := hosttelemetry.DefaultConfig()
	output := flag.String("output", "", "new private directory for the telemetry artifact")
	duration := flag.Duration("duration", defaults.Duration, "bounded collection duration (1s to 24h)")
	maxBytes := flag.Int64("max-bytes", defaults.MaxBytes, "maximum samples.ndjson bytes (1 MiB to 4 GiB)")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("positional arguments are not accepted")
	}
	defaults.OutputDir = *output
	defaults.Duration = *duration
	defaults.MaxBytes = *maxBytes

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	manifest, err := hosttelemetry.Run(ctx, defaults)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "host telemetry complete: samples=%d stop=%s\n", manifest.SampleCount, manifest.StopReason)
	return nil
}
