package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/infercrane/brezel/internal/rootdevice"
)

type phase struct {
	Name       string `json:"name"`
	DurationNS int64  `json:"duration_ns"`
	Passed     bool   `json:"passed"`
}

type report struct {
	SchemaVersion  int                   `json:"schema_version"`
	Kind           string                `json:"kind"`
	DiagnosticOnly bool                  `json:"diagnostic_only"`
	StartedAt      string                `json:"started_at"`
	FinishedAt     string                `json:"finished_at"`
	Filesystem     rootdevice.Filesystem `json:"filesystem"`
	Base           rootdevice.Base       `json:"base"`
	Iterations     int                   `json:"iterations"`
	Concurrency    int                   `json:"concurrency"`
	CloneNS        []int64               `json:"clone_ns"`
	CloneP50NS     int64                 `json:"clone_p50_ns"`
	CloneP95NS     int64                 `json:"clone_p95_ns"`
	Phases         []phase               `json:"phases"`
	Checks         map[string]bool       `json:"checks"`
	Passed         bool                  `json:"passed"`
	Error          string                `json:"error,omitempty"`
}

func measure(result *report, name string, operation func() error) error {
	started := time.Now()
	err := operation()
	result.Phases = append(result.Phases, phase{Name: name, DurationNS: time.Since(started).Nanoseconds(), Passed: err == nil})
	return err
}

func createBase(root string, bytes int64) (string, error) {
	path := filepath.Join(root, rootdevice.BaseName)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	completed := false
	defer func() {
		if completed {
			return
		}
		_ = file.Close()
		_ = os.Remove(path)
		_ = syncDirectory(root)
	}()
	hash := sha256.New()
	block := make([]byte, 1<<20)
	for index := range block {
		block[index] = byte((index*131 + 17) % 251)
	}
	remaining := bytes
	for remaining > 0 {
		current := int64(len(block))
		if current > remaining {
			current = remaining
		}
		if _, err := file.Write(block[:current]); err != nil {
			file.Close()
			return "", err
		}
		_, _ = hash.Write(block[:current])
		remaining -= current
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Chmod(0o400); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	directory, err := os.Open(root)
	if err != nil {
		return "", err
	}
	err = directory.Sync()
	_ = directory.Close()
	if err != nil {
		return "", err
	}
	completed = true
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func readPrefix(path string, size int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	buffer := make([]byte, size)
	if _, err := io.ReadFull(file, buffer); err != nil {
		return nil, err
	}
	return buffer, nil
}

func percentile(values []int64, fraction float64) int64 {
	copyValues := append([]int64(nil), values...)
	sort.Slice(copyValues, func(i, j int) bool { return copyValues[i] < copyValues[j] })
	index := int(float64(len(copyValues))*fraction+0.999999) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(copyValues) {
		index = len(copyValues) - 1
	}
	return copyValues[index]
}

func qualify(root string, iterations, concurrency int, baseBytes int64, diskFull bool) (result report, resultErr error) {
	result = report{SchemaVersion: 1, Kind: "brezel_reflink_rootdevice_qualification", DiagnosticOnly: true, StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Iterations: iterations, Concurrency: concurrency, Checks: map[string]bool{}}
	defer func() {
		result.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		result.Passed = resultErr == nil
		if resultErr != nil {
			result.Error = resultErr.Error()
		}
	}()
	if _, err := rootdevice.ValidateRootPath(root); err != nil {
		return result, err
	}
	if err := measure(&result, "filesystem_probe", func() error { var err error; result.Filesystem, err = rootdevice.Inspect(root); return err }); err != nil {
		return result, err
	}
	digest, err := createBase(root, baseBytes)
	if err != nil {
		return result, err
	}
	basePath := filepath.Join(root, rootdevice.BaseName)
	defer func() { _ = rootdevice.SetBaseImmutable(root, false); _ = os.Remove(basePath) }()
	if err := rootdevice.SetBaseImmutable(root, true); err != nil {
		return result, fmt.Errorf("set immutable base: %w", err)
	}
	if err := measure(&result, "immutable_base_validation", func() error { var err error; result.Base, err = rootdevice.ValidateBase(root, digest); return err }); err != nil {
		return result, err
	}
	result.Checks["immutable_base"] = true
	store, _, err := rootdevice.OpenStore(root, digest)
	if err != nil {
		return result, err
	}
	defer store.Close()

	if err := os.MkdirAll(filepath.Join(root, rootdevice.SandboxesName), 0o700); err != nil {
		return result, err
	}
	stale := filepath.Join(root, rootdevice.SandboxesName, ".clone-simulated-crash")
	if err := os.WriteFile(stale, []byte("unpublished"), 0o600); err != nil {
		return result, err
	}
	if err := measure(&result, "crash_recovery", func() error {
		removed, err := rootdevice.Recover(root)
		if err == nil && removed != 1 {
			return fmt.Errorf("recovered %d stale clones, want 1", removed)
		}
		return err
	}); err != nil {
		return result, err
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		return result, errors.New("stale pre-commit clone survived recovery")
	}
	result.Checks["crash_recovery"] = true

	outside := filepath.Join(root, "symlink-sentinel")
	if err := os.WriteFile(outside, []byte("sentinel"), 0o600); err != nil {
		return result, err
	}
	defer os.Remove(outside)
	attack := filepath.Join(root, rootdevice.SandboxesName, "symlink-attack.img")
	if err := os.Symlink(outside, attack); err != nil {
		return result, err
	}
	defer os.Remove(attack)
	if _, err := store.Clone("symlink-attack"); err == nil {
		return result, errors.New("clone replaced a protected-path symlink")
	}
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != "sentinel" {
		return result, errors.New("protected-path sentinel changed")
	}
	if err := os.Remove(attack); err != nil {
		return result, err
	}
	if err := os.Remove(outside); err != nil {
		return result, err
	}
	result.Checks["no_follow_no_replace"] = true

	type outcome struct {
		id    string
		clone rootdevice.CloneResult
		err   error
	}
	jobs := make(chan int)
	outcomes := make(chan outcome, iterations)
	var workers sync.WaitGroup
	for worker := 0; worker < concurrency; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				id := fmt.Sprintf("concurrent-%04d", index)
				clone, err := store.Clone(id)
				outcomes <- outcome{id: id, clone: clone, err: err}
			}
		}()
	}
	go func() {
		for index := 0; index < iterations; index++ {
			jobs <- index
		}
		close(jobs)
		workers.Wait()
		close(outcomes)
	}()
	var firstCloneErr error
	for outcome := range outcomes {
		if outcome.err != nil {
			if firstCloneErr == nil {
				firstCloneErr = fmt.Errorf("clone %s: %w", outcome.id, outcome.err)
			}
			continue
		}
		result.CloneNS = append(result.CloneNS, outcome.clone.DurationNS)
	}
	if firstCloneErr != nil {
		return result, firstCloneErr
	}
	sort.Slice(result.CloneNS, func(i, j int) bool { return result.CloneNS[i] < result.CloneNS[j] })
	result.CloneP50NS, result.CloneP95NS = percentile(result.CloneNS, .50), percentile(result.CloneNS, .95)
	result.Checks["concurrent_clone"] = true

	isolationPath, err := rootdevice.Destination(root, "concurrent-0000")
	if err != nil {
		return result, err
	}
	file, err := os.OpenFile(isolationPath, os.O_WRONLY, 0)
	if err != nil {
		return result, err
	}
	if _, err := file.WriteAt([]byte("CLONE"), 0); err != nil {
		file.Close()
		return result, err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return result, err
	}
	_ = file.Close()
	if _, err := rootdevice.ValidateBase(root, digest); err != nil {
		return result, fmt.Errorf("clone write changed base: %w", err)
	}
	if iterations > 1 {
		basePrefix, err := readPrefix(basePath, len("CLONE"))
		if err != nil {
			return result, err
		}
		siblingPath, err := rootdevice.Destination(root, "concurrent-0001")
		if err != nil {
			return result, err
		}
		siblingPrefix, err := readPrefix(siblingPath, len(basePrefix))
		if err != nil {
			return result, err
		}
		if !bytes.Equal(siblingPrefix, basePrefix) {
			return result, errors.New("clone write changed a sibling clone")
		}
	}
	result.Checks["clone_isolation"] = true

	for index := 0; index < iterations; index++ {
		if err := rootdevice.Delete(root, fmt.Sprintf("concurrent-%04d", index)); err != nil {
			return result, err
		}
	}
	if err := rootdevice.Delete(root, "concurrent-0000"); err != nil {
		return result, err
	}
	result.Checks["idempotent_delete"] = true
	if diskFull {
		if err := measure(&result, "dedicated_mount_disk_full", func() error { return rootdevice.ExerciseDiskFull(root, digest, "disk-full") }); err != nil {
			return result, err
		}
		result.Checks["disk_full"] = true
	}
	return result, nil
}

func main() {
	root := flag.String("root", "", "existing private qualification root")
	iterations := flag.Int("iterations", 32, "number of clone operations")
	concurrency := flag.Int("concurrency", 8, "concurrent clone workers")
	baseBytes := flag.Int64("base-bytes", 64<<20, "allocated immutable base bytes")
	diskFull := flag.Bool("disk-full", false, "fill an explicitly marked dedicated mount and verify ENOSPC behavior")
	flag.Parse()
	if flag.NArg() != 0 || *root == "" || *iterations < 1 || *iterations > 1000 || *concurrency < 1 || *concurrency > *iterations || *baseBytes < 1<<20 || *baseBytes > 1<<30 {
		fmt.Fprintln(os.Stderr, "invalid qualification arguments")
		os.Exit(2)
	}
	result, err := qualify(*root, *iterations, *concurrency, *baseBytes, *diskFull)
	encoded, encodeErr := json.MarshalIndent(result, "", "  ")
	if encodeErr != nil {
		fmt.Fprintln(os.Stderr, encodeErr)
		os.Exit(1)
	}
	fmt.Printf("%s\n", encoded)
	if err != nil {
		os.Exit(1)
	}
}
