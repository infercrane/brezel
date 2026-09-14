package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
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

func loadServiceToken() (string, error) {
	tokenFile := os.Getenv("RUNTIME_SERVICE_TOKEN_FILE")
	if tokenFile == "" {
		return "", errors.New("RUNTIME_SERVICE_TOKEN_FILE is required; the token is intentionally not accepted in argv or environment values")
	}
	token, err := readTokenFile(tokenFile)
	if err != nil {
		return "", fmt.Errorf("read runtime service token: %w", err)
	}
	return token, nil
}

func readTokenFile(path string) (string, error) {
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
