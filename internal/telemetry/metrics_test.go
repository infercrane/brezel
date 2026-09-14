package telemetry

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRegistryWritesBoundedPrometheusHistogram(t *testing.T) {
	registry := NewRegistry()
	registry.ObservePhase(OperationSandboxCreate, PhaseBackendCall, OutcomeSuccess, 250*time.Millisecond)
	registry.ObservePhase(OperationSandboxCreate, PhaseBackendCall, OutcomeSuccess, 2*time.Second)
	registry.ObservePhase(OperationCommand, PhaseGuestFirstEvent, OutcomeError, 5*time.Millisecond)

	var output bytes.Buffer
	if err := registry.WritePrometheus(&output); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, expected := range []string{
		`runtime_operation_phase_duration_seconds_bucket{operation="sandbox_create",phase="backend_call",outcome="success",le="0.5"} 1`,
		`runtime_operation_phase_duration_seconds_bucket{operation="sandbox_create",phase="backend_call",outcome="success",le="2.5"} 2`,
		`runtime_operation_phase_duration_seconds_count{operation="sandbox_create",phase="backend_call",outcome="success"} 2`,
		`runtime_operation_phase_duration_seconds_count{operation="command",phase="guest_first_event",outcome="error"} 1`,
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("metrics omitted %q:\n%s", expected, text)
		}
	}
}

func TestRegistryDropsUnknownDimensionsAndContent(t *testing.T) {
	registry := NewRegistry()
	secret := `project-a/sbx-secret/printf top-secret`
	registry.ObservePhase(Operation(secret), PhaseBackendCall, OutcomeSuccess, time.Second)
	registry.ObservePhase(OperationCommand, Phase(secret), OutcomeSuccess, time.Second)
	registry.ObservePhase(OperationCommand, PhaseBackendCall, Outcome(secret), time.Second)

	var output bytes.Buffer
	if err := registry.WritePrometheus(&output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), secret) || strings.Contains(output.String(), "_count{") {
		t.Fatalf("invalid dimensions reached metrics output: %s", output.String())
	}
}

func TestObserveClassifiesErrors(t *testing.T) {
	registry := NewRegistry()
	Observe(registry, OperationFileRead, PhaseGuestTransfer, time.Now(), errors.New("failed"))
	var output bytes.Buffer
	if err := registry.WritePrometheus(&output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `outcome="error"`) {
		t.Fatalf("error outcome omitted: %s", output.String())
	}
}
