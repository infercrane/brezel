package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/brezel/internal/telemetry"
)

func TestLoadNodeConfigRequiresTrustMaterialAndBoundsValues(t *testing.T) {
	values := map[string]string{
		"BREZEL_NODE_ID":                   "node-a",
		"BREZEL_NODE_API_ID":               "api-a",
		"BREZEL_NODE_LISTEN_ADDR":          "127.0.0.1:8443",
		"BREZEL_NODE_CONTROL_LISTEN_ADDR":  "127.0.0.1:8444",
		"BREZEL_NODE_CAPABILITY_KEYS_FILE": "/private/capability-keys.json",
		"BREZEL_NODE_TLS_CERT_FILE":        "/private/node.crt",
		"BREZEL_NODE_TLS_KEY_FILE":         "/private/node.key",
		"BREZEL_NODE_TLS_CA_FILE":          "/private/ca.crt",
		"BREZEL_NODE_LEDGER_FILE":          "/var/lib/brezel/node/routes.json",
		"BREZEL_ENGINE_TOKEN_FILE":         "/private/engine.token",
		"BREZEL_NODE_REPLAY_CAPACITY":      "4096",
		"BREZEL_NODE_MAX_IN_FLIGHT":        "128",
		"BREZEL_NODE_DRAIN_TIMEOUT":        "45s",
		"BREZEL_DURABLE_WORKSPACES":        "true",
	}
	for name, value := range values {
		t.Setenv(name, value)
	}
	config, err := loadNodeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.nodeID != "node-a" || config.apiID != "api-a" || config.replayCapacity != 4096 || config.maxInFlight != 128 || config.shutdownTimeout != 45*time.Second || !config.durableWorkspaces {
		t.Fatalf("unexpected config: %#v", config)
	}

	t.Setenv("BREZEL_NODE_REPLAY_CAPACITY", "0")
	if _, err := loadNodeConfig(); err == nil || !strings.Contains(err.Error(), "REPLAY_CAPACITY") {
		t.Fatalf("invalid replay capacity error=%v", err)
	}
	t.Setenv("BREZEL_NODE_REPLAY_CAPACITY", "4096")
	t.Setenv("BREZEL_NODE_DRAIN_TIMEOUT", "24h")
	if _, err := loadNodeConfig(); err == nil || !strings.Contains(err.Error(), "DRAIN_TIMEOUT") {
		t.Fatalf("invalid drain timeout error=%v", err)
	}
	t.Setenv("BREZEL_NODE_DRAIN_TIMEOUT", "45s")
	t.Setenv("BREZEL_NODE_MAX_IN_FLIGHT", "1025")
	if _, err := loadNodeConfig(); err == nil || !strings.Contains(err.Error(), "MAX_IN_FLIGHT") {
		t.Fatalf("invalid max in-flight error=%v", err)
	}
	t.Setenv("BREZEL_NODE_MAX_IN_FLIGHT", "128")
	t.Setenv("BREZEL_NODE_LEDGER_FILE", "relative/routes.json")
	if _, err := loadNodeConfig(); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative protected path error=%v", err)
	}
	t.Setenv("BREZEL_NODE_LEDGER_FILE", "/var/lib/brezel/node/routes.json")
	t.Setenv("BREZEL_NODE_CONTROL_LISTEN_ADDR", "127.0.0.1:8443")
	if _, err := loadNodeConfig(); err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("shared control/data listener error=%v", err)
	}
}

func TestErrorPhaseObserverLogsOnlyFailedClosedEnumPhase(t *testing.T) {
	var output bytes.Buffer
	observer := errorPhaseObserver{logger: log.New(&output, "", 0)}

	observer.ObservePhase(telemetry.OperationCommand, telemetry.PhaseGuestProcessStart, telemetry.OutcomeSuccess, 12*time.Millisecond)
	if output.Len() != 0 {
		t.Fatalf("successful phase unexpectedly logged: %q", output.String())
	}

	observer.ObservePhase(telemetry.OperationCommand, telemetry.PhaseGuestProcessRun, telemetry.OutcomeError, 17*time.Millisecond)
	if got, want := output.String(), "Brezel node backend phase failed operation=command phase=guest_process_run duration_ms=17\n"; got != want {
		t.Fatalf("log=%q want=%q", got, want)
	}
}

func TestLoadNodeConfigRejectsMissingIdentity(t *testing.T) {
	t.Setenv("BREZEL_NODE_ID", "")
	if _, err := loadNodeConfig(); err == nil || !strings.Contains(err.Error(), "BREZEL_NODE_ID") {
		t.Fatalf("missing node identity error=%v", err)
	}
}

func TestLoadCapabilityKeysRequiresCanonicalProtectedPolicy(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "capability-keys.json")
	policy := capabilityKeyPolicy{
		Version: 1,
		Issuer:  "brezel-api",
		Keys: []capabilityVerificationKey{{
			ID:              "api-key-a",
			PublicKeyBase64: base64.StdEncoding.EncodeToString(public),
		}},
	}
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	issuer, loaded, err := loadCapabilityKeys(path)
	if err != nil || issuer != policy.Issuer || !public.Equal(loaded["api-key-a"]) {
		t.Fatalf("issuer=%q loaded key matches=%v error=%v", issuer, public.Equal(loaded["api-key-a"]), err)
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadCapabilityKeys(path); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("public key permissions error=%v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "capability.link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadCapabilityKeys(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("public key symlink error=%v", err)
	}
	hardLink := filepath.Join(directory, "capability-hardlink.json")
	if err := os.Link(path, hardLink); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadCapabilityKeys(path); err == nil || !strings.Contains(err.Error(), "hard link") {
		t.Fatalf("public key hard-link error=%v", err)
	}
	if err := os.Remove(hardLink); err != nil {
		t.Fatal(err)
	}
	policy.Keys[0].PublicKeyBase64 = "not-a-key"
	data, err = json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadCapabilityKeys(path); err == nil || !strings.Contains(err.Error(), "base64") {
		t.Fatalf("invalid public key error=%v", err)
	}
}

func TestLoadCapabilityKeysRejectsDuplicateUnknownAndTrailingData(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(public)
	path := filepath.Join(t.TempDir(), "keys.json")
	write := func(value string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"version":1,"issuer":"brezel-api","keys":[{"id":"key-a","public_key_base64":"` + encoded + `"},{"id":"key-a","public_key_base64":"` + encoded + `"}]}`)
	if _, _, err := loadCapabilityKeys(path); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate key error=%v", err)
	}
	write(`{"version":1,"issuer":"brezel-api","keys":[{"id":"key-a","public_key_base64":"` + encoded + `"}],"unexpected":true}`)
	if _, _, err := loadCapabilityKeys(path); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field error=%v", err)
	}
	write(`{"version":1,"issuer":"brezel-api","keys":[{"id":"key-a","public_key_base64":"` + encoded + `"}]} {}`)
	if _, _, err := loadCapabilityKeys(path); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("trailing JSON error=%v", err)
	}
}
