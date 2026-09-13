package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/infercrane/sandbox-runtime-lab/internal/conformance"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	baseURL := flag.String("base-url", "http://127.0.0.1:8080", "runtime control API URL")
	project := flag.String("project", "conformance", "isolated conformance project")
	otherProject := flag.String("other-project", "", "second project used for isolation denial")
	template := flag.String("backend-template", "", "existing backend template ID")
	target := flag.String("target", "", "human-readable deployment identity")
	timeout := flag.Duration("timeout", 5*time.Minute, "whole-run timeout")
	execute := flag.Bool("execute", false, "create and delete real backend resources")
	flag.Parse()

	token := os.Getenv("RUNTIME_SERVICE_TOKEN")
	if token == "" {
		return errors.New("RUNTIME_SERVICE_TOKEN is required; it is intentionally not accepted as a CLI argument")
	}
	runner, err := conformance.New(conformance.Config{
		BaseURL: *baseURL, Token: token, ProjectID: *project,
		OtherProjectID: *otherProject, BackendTemplate: *template,
		Target: *target, Execute: *execute,
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	report, runErr := runner.Run(ctx)
	if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
		return err
	}
	if runErr != nil {
		return fmt.Errorf("conformance failed: %w", runErr)
	}
	return nil
}
