package telemetry

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// CommandDiagnosticComponent is a closed description of one command-stream
// hop. It cannot carry a project, sandbox, command, path, or content label.
type CommandDiagnosticComponent string

const (
	CommandDiagnosticPublicHandler CommandDiagnosticComponent = "public_handler"
	CommandDiagnosticRelayClient   CommandDiagnosticComponent = "relay_client"
	CommandDiagnosticRelayServer   CommandDiagnosticComponent = "relay_server"
	CommandDiagnosticGuestBackend  CommandDiagnosticComponent = "guest_backend"
)

// CommandDiagnostic contains monotonic durations and counters for one command
// at one fixed hop. Byte counts cover raw stdout and stderr payload bytes, not
// encoded wire bytes. The type deliberately has no extensible labels or IDs.
type CommandDiagnostic struct {
	Component            CommandDiagnosticComponent
	Outcome              Outcome
	AcceptedToFirstEvent time.Duration
	AcceptedToTerminal   time.Duration
	AcceptedToEOFReady   time.Duration
	RouteLookup          time.Duration
	FirstEventCount      uint64
	TerminalEventCount   uint64
	StreamEvents         uint64
	StreamBytes          uint64
}

// CommandDiagnosticObserver accepts only the fixed, content-free command
// diagnostic shape above.
type CommandDiagnosticObserver interface {
	ObserveCommandDiagnostic(CommandDiagnostic)
}

// CommandTrace records one diagnostic sample using Go's monotonic clock. A nil
// trace is a no-op, keeping the normal command path free of clock reads when
// diagnostics are disabled.
type CommandTrace struct {
	observer CommandDiagnosticObserver
	sample   CommandDiagnostic
	started  time.Time
}

func StartCommandTrace(observer CommandDiagnosticObserver, component CommandDiagnosticComponent) *CommandTrace {
	if observer == nil || !validCommandDiagnosticComponent(component) {
		return nil
	}
	return &CommandTrace{observer: observer, sample: CommandDiagnostic{Component: component}, started: time.Now()}
}

// ObserveEvent records only event and payload counts. It never receives the
// payload itself.
func (t *CommandTrace) ObserveEvent(payloadBytes int, terminal bool) {
	if t == nil {
		return
	}
	now := time.Now()
	t.sample.StreamEvents++
	if payloadBytes > 0 {
		t.sample.StreamBytes += uint64(payloadBytes)
	}
	if t.sample.FirstEventCount == 0 {
		t.sample.FirstEventCount = 1
		t.sample.AcceptedToFirstEvent = now.Sub(t.started)
	}
	if terminal && t.sample.TerminalEventCount == 0 {
		t.sample.TerminalEventCount = 1
		t.sample.AcceptedToTerminal = now.Sub(t.started)
	}
}

// StartInterval returns a monotonic start value only for an enabled trace.
func (t *CommandTrace) StartInterval() time.Time {
	if t == nil {
		return time.Time{}
	}
	return time.Now()
}

func (t *CommandTrace) ObserveRouteLookup(started time.Time) {
	if t == nil || started.IsZero() {
		return
	}
	t.sample.RouteLookup = time.Since(started)
}

// Finish publishes one sample. EOFReady means the current component has
// consumed its input stream and is ready to return; only a client-side timer
// can establish when the peer observed the network EOF.
func (t *CommandTrace) Finish(err error) {
	if t == nil {
		return
	}
	t.sample.AcceptedToEOFReady = time.Since(t.started)
	t.sample.Outcome = OutcomeSuccess
	if err != nil {
		t.sample.Outcome = OutcomeError
	}
	t.observer.ObserveCommandDiagnostic(t.sample)
}

// CommandDiagnosticLogger writes one JSON object per observation without a
// wall-clock timestamp, process identity, or caller-controlled label. It is
// intended only for explicitly enabled benchmark diagnostics.
type CommandDiagnosticLogger struct {
	mu     sync.Mutex
	writer io.Writer
}

func NewCommandDiagnosticLogger(writer io.Writer) *CommandDiagnosticLogger {
	if writer == nil {
		return nil
	}
	return &CommandDiagnosticLogger{writer: writer}
}

func (l *CommandDiagnosticLogger) ObserveCommandDiagnostic(sample CommandDiagnostic) {
	if l == nil || l.writer == nil || !validCommandDiagnosticComponent(sample.Component) || !validOutcome(sample.Outcome) {
		return
	}
	wire := commandDiagnosticWire{
		SchemaVersion:          1,
		Kind:                   "command",
		Component:              sample.Component,
		Outcome:                sample.Outcome,
		AcceptedToFirstEventNS: nonNegativeNanoseconds(sample.AcceptedToFirstEvent),
		AcceptedToTerminalNS:   nonNegativeNanoseconds(sample.AcceptedToTerminal),
		AcceptedToEOFReadyNS:   nonNegativeNanoseconds(sample.AcceptedToEOFReady),
		RouteLookupNS:          nonNegativeNanoseconds(sample.RouteLookup),
		FirstEventCount:        sample.FirstEventCount,
		TerminalEventCount:     sample.TerminalEventCount,
		StreamEvents:           sample.StreamEvents,
		StreamBytes:            sample.StreamBytes,
	}
	l.write(wire)
}

// ObservePhase makes the diagnostic logger usable alongside the ordinary
// fixed phase observer. Only command guest phases are accepted.
func (l *CommandDiagnosticLogger) ObservePhase(operation Operation, phase Phase, outcome Outcome, duration time.Duration) {
	if l == nil || l.writer == nil || operation != OperationCommand || !diagnosticCommandPhase(phase) || !validOutcome(outcome) {
		return
	}
	l.write(commandPhaseDiagnosticWire{
		SchemaVersion: 1,
		Kind:          "phase",
		Phase:         phase,
		Outcome:       outcome,
		DurationNS:    nonNegativeNanoseconds(duration),
	})
}

func (l *CommandDiagnosticLogger) write(value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return
	}
	encoded = append(encoded, '\n')
	l.mu.Lock()
	_, _ = l.writer.Write(encoded)
	l.mu.Unlock()
}

type commandDiagnosticWire struct {
	SchemaVersion          int                        `json:"schema_version"`
	Kind                   string                     `json:"kind"`
	Component              CommandDiagnosticComponent `json:"component"`
	Outcome                Outcome                    `json:"outcome"`
	AcceptedToFirstEventNS int64                      `json:"accepted_to_first_event_ns"`
	AcceptedToTerminalNS   int64                      `json:"accepted_to_terminal_ns"`
	AcceptedToEOFReadyNS   int64                      `json:"accepted_to_eof_ready_ns"`
	RouteLookupNS          int64                      `json:"route_lookup_ns"`
	FirstEventCount        uint64                     `json:"first_event_count"`
	TerminalEventCount     uint64                     `json:"terminal_event_count"`
	StreamEvents           uint64                     `json:"stream_events"`
	StreamBytes            uint64                     `json:"stream_bytes"`
}

type commandPhaseDiagnosticWire struct {
	SchemaVersion int     `json:"schema_version"`
	Kind          string  `json:"kind"`
	Phase         Phase   `json:"phase"`
	Outcome       Outcome `json:"outcome"`
	DurationNS    int64   `json:"duration_ns"`
}

func nonNegativeNanoseconds(duration time.Duration) int64 {
	if duration < 0 {
		return 0
	}
	return duration.Nanoseconds()
}

func validCommandDiagnosticComponent(component CommandDiagnosticComponent) bool {
	switch component {
	case CommandDiagnosticPublicHandler, CommandDiagnosticRelayClient, CommandDiagnosticRelayServer, CommandDiagnosticGuestBackend:
		return true
	default:
		return false
	}
}

func diagnosticCommandPhase(phase Phase) bool {
	switch phase {
	case PhaseGuestConnection, PhaseGuestProcessStart, PhaseGuestFirstEvent, PhaseGuestProcessRun:
		return true
	default:
		return false
	}
}

type joinedObserver []Observer

// JoinObservers fans a fixed phase observation out to non-nil observers.
// Returning nil for an empty input preserves the disabled fast path.
func JoinObservers(observers ...Observer) Observer {
	joined := make(joinedObserver, 0, len(observers))
	for _, observer := range observers {
		if observer != nil {
			joined = append(joined, observer)
		}
	}
	switch len(joined) {
	case 0:
		return nil
	case 1:
		return joined[0]
	default:
		return joined
	}
}

func (o joinedObserver) ObservePhase(operation Operation, phase Phase, outcome Outcome, duration time.Duration) {
	for _, observer := range o {
		observer.ObservePhase(operation, phase, outcome, duration)
	}
}
