package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadServiceTokenRequiresPrivateRegularFile(t *testing.T) {
	directory := t.TempDir()
	private := filepath.Join(directory, "service.token")
	if err := os.WriteFile(private, []byte("0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RUNTIME_SERVICE_TOKEN_FILE", private)
	token, err := loadServiceToken()
	if err != nil || token != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("loadServiceToken() = %q, %v", token, err)
	}
	public := filepath.Join(directory, "public.token")
	if err := os.WriteFile(public, []byte("0123456789abcdef0123456789abcdef"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RUNTIME_SERVICE_TOKEN_FILE", public)
	if _, err := loadServiceToken(); err == nil {
		t.Fatal("world-readable token was accepted")
	}
}

func TestRunRequiresExplicitExecutionBeforeCredentials(t *testing.T) {
	t.Setenv("RUNTIME_SERVICE_TOKEN_FILE", "")
	err := run(nil, &strings.Builder{}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "explicit") {
		t.Fatalf("run() error = %v", err)
	}
}

func TestRunRejectsOutOfRangeScenarioParametersBeforeCredentials(t *testing.T) {
	t.Setenv("RUNTIME_SERVICE_TOKEN_FILE", "")
	for _, args := range [][]string{
		{"-execute", "-io-bytes", "0"},
		{"-execute", "-io-bytes", "33554433"},
		{"-execute", "-preview-port", "0"},
		{"-execute", "-preview-port", "65536"},
	} {
		if err := run(args, &strings.Builder{}, &strings.Builder{}); err == nil || (!strings.Contains(err.Error(), "io-bytes") && !strings.Contains(err.Error(), "preview-port")) {
			t.Fatalf("run(%v) error = %v", args, err)
		}
	}
}
