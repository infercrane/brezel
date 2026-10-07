// Package qualification verifies the exact-revision evidence that a running
// Brezel API is allowed to advertise. A source build or stale receipt remains
// unverified; qualification is never inferred from feature flags alone.
package qualification

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/infercrane/brezel/internal/securefile"
)

const (
	maxReceiptBytes = 64 << 10
	maxReportBytes  = 4 << 20
)

var (
	hexRevision     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	hexDigest       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	requiredReports = map[string]bool{
		"conformance":        true,
		"engine_fast_path":   true,
		"controller_restart": true,
		"node_restart":       true,
		"post_restart":       true,
	}
)

type Report struct {
	Name   string `json:"name"`
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
}

type Receipt struct {
	SchemaVersion   int       `json:"schema_version"`
	Qualification   string    `json:"qualification"`
	RuntimeRevision string    `json:"runtime_revision"`
	QualifiedAt     time.Time `json:"qualified_at"`
	Reports         []Report  `json:"reports"`
}

type Status struct {
	Qualification string
	Note          string
}

// Read verifies a private, atomically published receipt and each referenced
// report. The receipt is useful only for the exact immutable binary revision.
func Read(path, runningRevision string, now time.Time) (Status, error) {
	path = strings.TrimSpace(path)
	runningRevision = strings.TrimSpace(runningRevision)
	if path == "" {
		return Status{}, errors.New("qualification receipt path is not configured")
	}
	if !hexRevision.MatchString(runningRevision) {
		return Status{}, errors.New("running revision is not an immutable full Git revision")
	}
	data, err := securefile.Read(path, maxReceiptBytes)
	if err != nil {
		return Status{}, fmt.Errorf("read qualification receipt: %w", err)
	}
	var receipt Receipt
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&receipt); err != nil {
		return Status{}, fmt.Errorf("decode qualification receipt: %w", err)
	}
	if err = decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Status{}, errors.New("qualification receipt contains multiple JSON values")
	}
	if receipt.SchemaVersion != 1 || receipt.Qualification != "sandbox_runtime_conformant" {
		return Status{}, errors.New("qualification receipt has an unsupported contract")
	}
	if receipt.RuntimeRevision != runningRevision {
		return Status{}, errors.New("qualification receipt does not match the running revision")
	}
	if receipt.QualifiedAt.IsZero() || receipt.QualifiedAt.After(now.Add(5*time.Minute)) {
		return Status{}, errors.New("qualification receipt has an invalid qualification time")
	}
	if len(receipt.Reports) != len(requiredReports) {
		return Status{}, errors.New("qualification receipt does not contain the complete evidence set")
	}
	seen := make(map[string]bool, len(receipt.Reports))
	for _, report := range receipt.Reports {
		if !requiredReports[report.Name] || seen[report.Name] {
			return Status{}, errors.New("qualification receipt contains an unknown or duplicate report")
		}
		seen[report.Name] = true
		if report.File == "" || filepath.Base(report.File) != report.File || report.File == "." || strings.ContainsAny(report.File, "\x00\r\n") {
			return Status{}, errors.New("qualification receipt contains an unsafe report path")
		}
		if !hexDigest.MatchString(report.SHA256) {
			return Status{}, errors.New("qualification receipt contains an invalid report digest")
		}
		reportData, readErr := securefile.Read(filepath.Join(filepath.Dir(path), report.File), maxReportBytes)
		if readErr != nil {
			return Status{}, fmt.Errorf("read qualification report %q: %w", report.Name, readErr)
		}
		digest := sha256.Sum256(reportData)
		if hex.EncodeToString(digest[:]) != report.SHA256 {
			return Status{}, fmt.Errorf("qualification report %q digest does not match", report.Name)
		}
	}
	return Status{
		Qualification: receipt.Qualification,
		Note:          fmt.Sprintf("Exact runtime revision %s qualified at %s.", runningRevision[:12], receipt.QualifiedAt.UTC().Format(time.RFC3339)),
	}, nil
}
