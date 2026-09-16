package telemetry

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func TestCommandDiagnosticLoggerEmitsOnlyClosedDurationsAndCounters(t *testing.T) {
	var output bytes.Buffer
	logger := NewCommandDiagnosticLogger(&output)
	logger.ObserveCommandDiagnostic(CommandDiagnostic{
		Component:            CommandDiagnosticRelayClient,
		Outcome:              OutcomeSuccess,
		AcceptedToFirstEvent: 3 * time.Millisecond,
		AcceptedToTerminal:   7 * time.Millisecond,
		AcceptedToEOFReady:   9 * time.Millisecond,
		RouteLookup:          time.Millisecond,
		FirstEventCount:      1,
		TerminalEventCount:   1,
		StreamEvents:         4,
		StreamBytes:          128,
	})

	var got map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{
		"schema_version":             1,
		"accepted_to_first_event_ns": 3_000_000,
		"accepted_to_terminal_ns":    7_000_000,
		"accepted_to_eof_ready_ns":   9_000_000,
		"route_lookup_ns":            1_000_000,
		"first_event_count":          1,
		"terminal_event_count":       1,
		"stream_events":              4,
		"stream_bytes":               128,
	}
	for key, expected := range want {
		if got[key] != expected {
			t.Fatalf("%s=%v want %v in %s", key, got[key], expected, output.String())
		}
	}
	if got["kind"] != "command" || got["component"] != "relay_client" || got["outcome"] != "success" {
		t.Fatalf("fixed dimensions=%#v", got)
	}
	for _, forbidden := range []string{"project", "sandbox", "command", "path", "content", "execution_id", "timestamp"} {
		if _, exists := got[forbidden]; exists {
			t.Fatalf("forbidden field %q reached diagnostic output: %s", forbidden, output.String())
		}
	}
}

func TestCommandDiagnosticLoggerDropsUnknownDimensionsAndNonCommandPhases(t *testing.T) {
	var output bytes.Buffer
	logger := NewCommandDiagnosticLogger(&output)
	logger.ObserveCommandDiagnostic(CommandDiagnostic{Component: CommandDiagnosticComponent("private-resource"), Outcome: OutcomeSuccess})
	logger.ObservePhase(OperationFileRead, PhaseGuestConnection, OutcomeHit, time.Millisecond)
	logger.ObservePhase(OperationCommand, Phase("private-command"), OutcomeHit, time.Millisecond)
	if output.Len() != 0 {
		t.Fatalf("unknown diagnostic dimensions were emitted: %s", output.String())
	}

	logger.ObservePhase(OperationCommand, PhaseGuestConnection, OutcomeMiss, -time.Millisecond)
	var got map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &got); err != nil {
		t.Fatal(err)
	}
	if got["kind"] != "phase" || got["phase"] != "guest_connection" || got["outcome"] != "miss" || got["duration_ns"] != float64(0) {
		t.Fatalf("phase diagnostic=%#v", got)
	}
}

func TestCommandTraceUsesCountersToDistinguishMissingEvents(t *testing.T) {
	collector := &commandDiagnosticCollector{}
	trace := StartCommandTrace(collector, CommandDiagnosticGuestBackend)
	trace.ObserveEvent(0, false)
	trace.ObserveEvent(5, false)
	trace.ObserveEvent(0, true)
	trace.Finish(nil)

	if len(collector.samples) != 1 {
		t.Fatalf("samples=%d", len(collector.samples))
	}
	sample := collector.samples[0]
	if sample.StreamEvents != 3 || sample.StreamBytes != 5 || sample.FirstEventCount != 1 || sample.TerminalEventCount != 1 {
		t.Fatalf("sample=%#v", sample)
	}
	if sample.AcceptedToFirstEvent < 0 || sample.AcceptedToTerminal < sample.AcceptedToFirstEvent || sample.AcceptedToEOFReady < sample.AcceptedToTerminal {
		t.Fatalf("non-monotonic sample=%#v", sample)
	}

	var disabled CommandDiagnosticObserver
	if StartCommandTrace(disabled, CommandDiagnosticPublicHandler) != nil {
		t.Fatal("disabled diagnostics allocated a trace")
	}
}

func TestJoinObserversPreservesEmptyAndFansOut(t *testing.T) {
	if JoinObservers(nil, nil) != nil {
		t.Fatal("empty observer join did not preserve disabled state")
	}
	first := &phaseCollector{}
	second := &phaseCollector{}
	joined := JoinObservers(first, nil, second)
	joined.ObservePhase(OperationCommand, PhaseGuestConnection, OutcomeHit, time.Millisecond)
	if first.count != 1 || second.count != 1 {
		t.Fatalf("fanout counts=%d,%d", first.count, second.count)
	}
}

type commandDiagnosticCollector struct {
	samples []CommandDiagnostic
}

func (c *commandDiagnosticCollector) ObserveCommandDiagnostic(sample CommandDiagnostic) {
	c.samples = append(c.samples, sample)
}

type phaseCollector struct {
	count int
}

func (c *phaseCollector) ObservePhase(Operation, Phase, Outcome, time.Duration) {
	c.count++
}
