package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadTokenFileRequiresPrivateRegularFile(t *testing.T) {
	directory := t.TempDir()
	private := filepath.Join(directory, "service.token")
	if err := os.WriteFile(private, []byte("0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := readTokenFile(private)
	if err != nil || token != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("readTokenFile() = %q, %v", token, err)
	}
	public := filepath.Join(directory, "public.token")
	if err := os.WriteFile(public, []byte("0123456789abcdef0123456789abcdef"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readTokenFile(public); err == nil {
		t.Fatal("world-readable token was accepted")
	}
}

func TestLoadServiceTokenDoesNotReadTokenValueEnvironment(t *testing.T) {
	t.Setenv("BREZEL_SERVICE_TOKEN", "0123456789abcdef0123456789abcdef")
	t.Setenv("BREZEL_SERVICE_TOKEN_FILE", "")
	_, err := loadServiceToken()
	if err == nil || !strings.Contains(err.Error(), "TOKEN_FILE") {
		t.Fatalf("loadServiceToken() error = %v, want protected file requirement", err)
	}
}
