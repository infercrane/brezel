package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSecretFileRequiresPrivateRegularFile(t *testing.T) {
	directory := t.TempDir()
	private := filepath.Join(directory, "private")
	if err := os.WriteFile(private, []byte(" secret-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	value, err := loadSecretFile(private)
	if err != nil || value != "secret-value" {
		t.Fatalf("loadSecretFile() = %q, %v", value, err)
	}
	public := filepath.Join(directory, "public")
	if err := os.WriteFile(public, []byte("secret-value"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSecretFile(public); err == nil {
		t.Fatal("world-readable secret was accepted")
	}
	if _, err := loadSecretFile(directory); err == nil {
		t.Fatal("directory was accepted as a secret")
	}
}

func TestParseBoolEnvRejectsInvalidCapabilityConfiguration(t *testing.T) {
	t.Setenv("BREZEL_DURABLE_WORKSPACES", "sometimes")
	if _, err := parseBoolEnv("BREZEL_DURABLE_WORKSPACES", false); err == nil {
		t.Fatal("invalid capability configuration was accepted")
	}
	t.Setenv("BREZEL_DURABLE_WORKSPACES", "true")
	value, err := parseBoolEnv("BREZEL_DURABLE_WORKSPACES", false)
	if err != nil || !value {
		t.Fatalf("parseBoolEnv() = %v, %v", value, err)
	}
}

func TestLoadLimitsRejectsUnboundedOrMalformedValues(t *testing.T) {
	t.Setenv("BREZEL_MAX_ACTIVE_SANDBOXES_PER_PROJECT", "0")
	if _, err := loadLimits(); err == nil {
		t.Fatal("zero sandbox limit was accepted")
	}
	t.Setenv("BREZEL_MAX_ACTIVE_SANDBOXES_PER_PROJECT", "5")
	t.Setenv("BREZEL_MAX_WORKSPACES_PER_PROJECT", "nope")
	if _, err := loadLimits(); err == nil {
		t.Fatal("malformed workspace limit was accepted")
	}
	t.Setenv("BREZEL_MAX_WORKSPACES_PER_PROJECT", "6")
	t.Setenv("BREZEL_MAX_CONCURRENT_GUEST_OPS_PER_PROJECT", "7")
	t.Setenv("BREZEL_MAX_ENVIRONMENTS_PER_PROJECT", "8")
	t.Setenv("BREZEL_MAX_CONNECTORS_PER_PROJECT", "9")
	limits, err := loadLimits()
	if err != nil {
		t.Fatal(err)
	}
	if limits.MaxActiveSandboxesPerProject != 5 || limits.MaxWorkspacesPerProject != 6 || limits.MaxConcurrentGuestOpsPerProject != 7 || limits.MaxEnvironmentsPerProject != 8 || limits.MaxConnectorsPerProject != 9 {
		t.Fatalf("limits = %#v", limits)
	}
}
