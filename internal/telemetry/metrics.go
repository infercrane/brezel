// Package telemetry provides content-free, low-cardinality runtime metrics.
//
// Labels are deliberately closed enums. Callers cannot attach project,
// resource, path, command, or customer-content dimensions to observations.
package telemetry

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"sync"
	"time"
)

type Operation string

const (
	OperationSandboxCreate     Operation = "sandbox_create"
	OperationSandboxPause      Operation = "sandbox_pause"
	OperationSandboxResume     Operation = "sandbox_resume"
	OperationSandboxDelete     Operation = "sandbox_delete"
	OperationSandboxCheckpoint Operation = "sandbox_checkpoint"
	OperationSandboxObserve    Operation = "sandbox_observe"
	OperationCommand           Operation = "command"
	OperationFileRead          Operation = "file_read"
	OperationFileWrite         Operation = "file_write"
)

type Phase string

const (
	PhasePersistIntent     Phase = "persist_intent"
	PhaseBackendCall       Phase = "backend_call"
	PhasePersistResult     Phase = "persist_result"
	PhaseGuestAcquire      Phase = "guest_acquire"
	PhaseGuestConnection   Phase = "guest_connection"
	PhaseGuestProcessStart Phase = "guest_process_start"
	PhaseGuestFirstEvent   Phase = "guest_first_event"
	PhaseGuestProcessRun   Phase = "guest_process_run"
	PhaseGuestTransfer     Phase = "guest_transfer"
	PhaseGuestHealth       Phase = "guest_health"
)

type Outcome string

const (
	OutcomeSuccess Outcome = "success"
	OutcomeError   Outcome = "error"
	OutcomeHit     Outcome = "hit"
	OutcomeMiss    Outcome = "miss"
)

type Observer interface {
	ObservePhase(Operation, Phase, Outcome, time.Duration)
}

// Registry stores a fixed-dimensional histogram in process memory. It is
// suitable for the single-host profile and safe for concurrent observation.
type Registry struct {
	mu      sync.Mutex
	metrics map[metricKey]*histogram
}

type metricKey struct {
	operation Operation
	phase     Phase
	outcome   Outcome
}

type histogram struct {
	count   uint64
	sum     float64
	buckets []uint64
}

var bucketUpperBounds = [...]float64{
	0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25,
	0.5, 1, 2.5, 5, 10, 30, 60,
}

func NewRegistry() *Registry {
	return &Registry{metrics: make(map[metricKey]*histogram)}
}

func (r *Registry) ObservePhase(operation Operation, phase Phase, outcome Outcome, duration time.Duration) {
	if r == nil || !validOperation(operation) || !validPhase(phase) || !validOutcome(outcome) {
		return
	}
	if duration < 0 {
		duration = 0
	}
	seconds := duration.Seconds()
	key := metricKey{operation: operation, phase: phase, outcome: outcome}
	r.mu.Lock()
	metric := r.metrics[key]
	if metric == nil {
		metric = &histogram{buckets: make([]uint64, len(bucketUpperBounds))}
		r.metrics[key] = metric
	}
	metric.count++
	metric.sum += seconds
	for index, upper := range bucketUpperBounds {
		if seconds <= upper {
			metric.buckets[index]++
		}
	}
	r.mu.Unlock()
}

// WritePrometheus writes the OpenMetrics-compatible Prometheus text format.
// It snapshots under the lock, then performs I/O without blocking observers.
func (r *Registry) WritePrometheus(destination io.Writer) error {
	if r == nil {
		return nil
	}
	type sample struct {
		key     metricKey
		count   uint64
		sum     float64
		buckets []uint64
	}
	r.mu.Lock()
	samples := make([]sample, 0, len(r.metrics))
	for key, metric := range r.metrics {
		samples = append(samples, sample{key: key, count: metric.count, sum: metric.sum, buckets: append([]uint64(nil), metric.buckets...)})
	}
	r.mu.Unlock()
	sort.Slice(samples, func(i, j int) bool {
		left, right := samples[i].key, samples[j].key
		if left.operation != right.operation {
			return left.operation < right.operation
		}
		if left.phase != right.phase {
			return left.phase < right.phase
		}
		return left.outcome < right.outcome
	})
	if _, err := io.WriteString(destination, "# HELP runtime_operation_phase_duration_seconds Content-free duration of fixed runtime operation phases.\n# TYPE runtime_operation_phase_duration_seconds histogram\n"); err != nil {
		return err
	}
	for _, current := range samples {
		labels := fmt.Sprintf("operation=%q,phase=%q,outcome=%q", current.key.operation, current.key.phase, current.key.outcome)
		for index, upper := range bucketUpperBounds {
			if _, err := fmt.Fprintf(destination, "runtime_operation_phase_duration_seconds_bucket{%s,le=%q} %d\n", labels, strconv.FormatFloat(upper, 'g', -1, 64), current.buckets[index]); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(destination, "runtime_operation_phase_duration_seconds_bucket{%s,le=\"+Inf\"} %d\nruntime_operation_phase_duration_seconds_sum{%s} %s\nruntime_operation_phase_duration_seconds_count{%s} %d\n", labels, current.count, labels, strconv.FormatFloat(current.sum, 'g', -1, 64), labels, current.count); err != nil {
			return err
		}
	}
	return nil
}

func Observe(observer Observer, operation Operation, phase Phase, started time.Time, err error) {
	if observer == nil {
		return
	}
	outcome := OutcomeSuccess
	if err != nil {
		outcome = OutcomeError
	}
	observer.ObservePhase(operation, phase, outcome, time.Since(started))
}

func validOperation(value Operation) bool {
	switch value {
	case OperationSandboxCreate, OperationSandboxPause, OperationSandboxResume, OperationSandboxDelete,
		OperationSandboxCheckpoint, OperationSandboxObserve, OperationCommand, OperationFileRead, OperationFileWrite:
		return true
	default:
		return false
	}
}

func validPhase(value Phase) bool {
	switch value {
	case PhasePersistIntent, PhaseBackendCall, PhasePersistResult, PhaseGuestAcquire,
		PhaseGuestConnection, PhaseGuestProcessStart, PhaseGuestFirstEvent,
		PhaseGuestProcessRun, PhaseGuestTransfer, PhaseGuestHealth:
		return true
	default:
		return false
	}
}

func validOutcome(value Outcome) bool {
	switch value {
	case OutcomeSuccess, OutcomeError, OutcomeHit, OutcomeMiss:
		return true
	default:
		return false
	}
}
