package qualification

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeEvidence(t *testing.T, revision string) string {
	t.Helper()
	directory := t.TempDir()
	reports := make([]Report, 0, len(requiredReports))
	for name := range requiredReports {
		file := name + ".json"
		data := []byte(`{"outcome":"passed"}`)
		if err := os.WriteFile(filepath.Join(directory, file), data, 0o600); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(data)
		reports = append(reports, Report{Name: name, File: file, SHA256: hex.EncodeToString(digest[:])})
	}
	receipt := Receipt{SchemaVersion: 1, Qualification: "sandbox_runtime_conformant", RuntimeRevision: revision, QualifiedAt: time.Date(2026, time.October, 7, 0, 0, 0, 0, time.UTC), Reports: reports}
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "current.json")
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadAcceptsCompleteExactRevisionEvidence(t *testing.T) {
	revision := "0123456789abcdef0123456789abcdef01234567"
	path := writeEvidence(t, revision)
	status, err := Read(path, revision, time.Date(2026, time.October, 7, 1, 0, 0, 0, time.UTC))
	if err != nil || status.Qualification != "sandbox_runtime_conformant" || status.Note == "" {
		t.Fatalf("Read() = %#v, %v", status, err)
	}
}

func TestReadRejectsStaleRevisionAndChangedReport(t *testing.T) {
	revision := "0123456789abcdef0123456789abcdef01234567"
	path := writeEvidence(t, revision)
	if _, err := Read(path, "1123456789abcdef0123456789abcdef01234567", time.Now()); err == nil {
		t.Fatal("expected stale revision rejection")
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "conformance.json"), []byte(`{"outcome":"changed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path, revision, time.Now()); err == nil {
		t.Fatal("expected changed report rejection")
	}
}

func TestReadRejectsSourceBuildAndIncompleteReceipt(t *testing.T) {
	revision := "0123456789abcdef0123456789abcdef01234567"
	path := writeEvidence(t, revision)
	if _, err := Read(path, "unknown", time.Now()); err == nil {
		t.Fatal("expected source build rejection")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var receipt Receipt
	if err = json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	receipt.Reports = receipt.Reports[:len(receipt.Reports)-1]
	data, _ = json.Marshal(receipt)
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = Read(path, revision, time.Now()); err == nil {
		t.Fatal("expected incomplete evidence rejection")
	}
}
