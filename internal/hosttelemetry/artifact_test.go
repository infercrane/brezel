package hosttelemetry

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArtifactIsBoundedPrivateDurableAndChecksummed(t *testing.T) {
	output := filepath.Join(t.TempDir(), "host-evidence")
	writer, err := newArtifactWriter(output, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	sample := Sample{SchemaVersion: SchemaVersion, Sequence: 1, SampledAt: "2026-09-16T10:00:00Z", ElapsedNS: int64(time.Second), CPU: CPU{LogicalCPUs: 44, DeltaTotal: 4400, DeltaSteal: 2}}
	if err := writer.append(sample); err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	manifest, err := writer.finalize(started, started.Add(time.Second), time.Second, "duration_complete", false)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SampleCount != 1 || manifest.IntervalNS != int64(time.Second) || manifest.StopReason != "duration_complete" || manifest.Truncated {
		t.Fatalf("manifest=%#v", manifest)
	}
	samples, err := os.ReadFile(filepath.Join(output, samplesFilename))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(samples)
	if got := hex.EncodeToString(digest[:]); got != manifest.SamplesSHA256 {
		t.Fatalf("samples digest=%s manifest=%s", got, manifest.SamplesSHA256)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(output, manifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	var decoded Manifest
	if err := json.Unmarshal(manifestBytes, &decoded); err != nil || decoded != manifest {
		t.Fatalf("decoded manifest=%#v error=%v", decoded, err)
	}
	checksums, err := os.ReadFile(filepath.Join(output, checksumsFilename))
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := sha256.Sum256(manifestBytes)
	for _, expected := range []string{manifest.SamplesSHA256 + "  " + samplesFilename, hex.EncodeToString(manifestDigest[:]) + "  " + manifestFilename} {
		if !strings.Contains(string(checksums), expected) {
			t.Fatalf("checksum %q missing from %s", expected, checksums)
		}
	}
	for _, name := range []string{samplesFilename, manifestFilename, checksumsFilename} {
		info, err := os.Stat(filepath.Join(output, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s permissions=%o", name, info.Mode().Perm())
		}
	}
	if entries, err := os.ReadDir(output); err != nil || len(entries) != 3 {
		t.Fatalf("artifact entries=%#v error=%v", entries, err)
	}
	if info, err := os.Stat(output); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("artifact directory permissions=%o", info.Mode().Perm())
	}
	scanner := bufio.NewScanner(strings.NewReader(string(samples)))
	if !scanner.Scan() || scanner.Scan() || scanner.Err() != nil {
		t.Fatalf("samples are not one complete NDJSON row: %q error=%v", samples, scanner.Err())
	}
}

func TestArtifactStopsBeforeCrossingOutputLimit(t *testing.T) {
	output := filepath.Join(t.TempDir(), "bounded")
	writer, err := newArtifactWriter(output, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.append(Sample{SchemaVersion: SchemaVersion}); !errors.Is(err, errOutputLimit) {
		t.Fatalf("append error=%v", err)
	}
	started := time.Now().UTC()
	manifest, err := writer.finalize(started, started, 0, "output_limit", true)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SampleCount != 0 || manifest.SampleBytes != 0 || !manifest.Truncated || manifest.SamplesSHA256 != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("manifest=%#v", manifest)
	}
}

func TestConfigRejectsUnboundedAndUnsafeInputs(t *testing.T) {
	config := DefaultConfig()
	config.OutputDir = filepath.Join(t.TempDir(), "output")
	if err := config.validate(); err != nil {
		t.Fatal(err)
	}
	config.Duration = 25 * time.Hour
	if err := config.validate(); err == nil {
		t.Fatal("unbounded duration was accepted")
	}
	config = DefaultConfig()
	config.OutputDir = filepath.Join(t.TempDir(), "output")
	config.ProcessNames = []string{"brezeld", "private/customer"}
	if err := config.validate(); err == nil {
		t.Fatal("unsafe process name was accepted")
	}
}
