package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/infercrane/brezel/internal/conformance"
	"github.com/infercrane/brezel/internal/securefile"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(parent context.Context) error {
	baseURL := flag.String("base-url", "http://127.0.0.1:8080", "runtime control API URL")
	project := flag.String("project", "conformance", "isolated conformance project")
	otherProject := flag.String("other-project", "", "second project used for isolation denial")
	template := flag.String("backend-template", "", "existing backend template ID")
	target := flag.String("target", "", "human-readable deployment identity")
	timeout := flag.Duration("timeout", 5*time.Minute, "whole-run timeout")
	execute := flag.Bool("execute", false, "create and delete real backend resources")
	flag.Parse()

	token, err := loadServiceToken()
	if err != nil {
		return err
	}
	runner, err := conformance.New(conformance.Config{
		BaseURL: *baseURL, Token: token, ProjectID: *project,
		OtherProjectID: *otherProject, BackendTemplate: *template,
		Target: *target, Execute: *execute,
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, *timeout)
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

func loadServiceToken() (string, error) {
	tokenFile := os.Getenv("BREZEL_SERVICE_TOKEN_FILE")
	if tokenFile == "" {
		return "", errors.New("BREZEL_SERVICE_TOKEN_FILE is required; the token is intentionally not accepted in argv or environment values")
	}
	token, err := readTokenFile(tokenFile)
	if err != nil {
		return "", fmt.Errorf("read runtime service token: %w", err)
	}
	return token, nil
}

func readTokenFile(path string) (string, error) {
	token, err := securefile.ReadText(path, 16<<10)
	if err != nil {
		return "", err
	}
	if len(token) < 32 {
		return "", errors.New("token file does not contain a valid service token")
	}
	return token, nil
}
