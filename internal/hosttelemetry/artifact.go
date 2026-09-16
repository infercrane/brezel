package hosttelemetry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"runtime"
	"time"
)

const (
	samplesFilename   = "samples.ndjson"
	manifestFilename  = "manifest.json"
	checksumsFilename = "SHA256SUMS"
)

var errOutputLimit = errors.New("host telemetry output limit reached")

// Run collects one bounded artifact. Cancellation is a graceful stop: the
// samples are synced and a checksummed manifest is still written.
func Run(ctx context.Context, config Config) (Manifest, error) {
	if runtime.GOOS != "linux" {
		return Manifest{}, errors.New("host telemetry collection requires Linux")
	}
	collector, err := newCollector(config)
	if err != nil {
		return Manifest{}, err
	}
	if err := collector.baselineCPU(); err != nil {
		return Manifest{}, errors.New("could not establish CPU baseline")
	}
	writer, err := newArtifactWriter(config.OutputDir, config.MaxBytes)
	if err != nil {
		return Manifest{}, err
	}
	started := time.Now()
	startedUTC := started.UTC()
	ticker := time.NewTicker(config.Interval)
	defer ticker.Stop()
	timer := time.NewTimer(config.Duration)
	defer timer.Stop()
	stopReason := "duration_complete"
	truncated := false
	var runErr error
	var sequence uint64

collectLoop:
	for {
		select {
		case <-ctx.Done():
			stopReason = "context_canceled"
			break collectLoop
		case <-timer.C:
			break collectLoop
		case <-ticker.C:
			sample := collector.collect(sequence, started)
			if err := writer.append(sample); err != nil {
				if errors.Is(err, errOutputLimit) {
					stopReason = "output_limit"
					truncated = true
				} else {
					stopReason = "write_error"
					runErr = err
				}
				break collectLoop
			}
			sequence++
		}
	}

	manifest, finalizeErr := writer.finalize(startedUTC, time.Now().UTC(), time.Since(started), config.Interval, stopReason, truncated)
	return manifest, errors.Join(runErr, finalizeErr)
}

type artifactWriter struct {
	directory string
	file      *os.File
	digest    hash.Hash
	maxBytes  int64
	written   int64
	samples   uint64
}

func newArtifactWriter(directory string, maxBytes int64) (*artifactWriter, error) {
	if err := os.Mkdir(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create telemetry output directory: %w", err)
	}
	partPath := directory + string(os.PathSeparator) + samplesFilename + ".part"
	file, err := os.OpenFile(partPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create telemetry samples: %w", err)
	}
	return &artifactWriter{directory: directory, file: file, digest: sha256.New(), maxBytes: maxBytes}, nil
}

func (w *artifactWriter) append(sample Sample) error {
	encoded, err := json.Marshal(sample)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if int64(len(encoded)) > w.maxBytes-w.written {
		return errOutputLimit
	}
	before := w.written
	written, writeErr := w.file.Write(encoded)
	if writeErr != nil || written != len(encoded) {
		if truncateErr := w.file.Truncate(before); truncateErr != nil {
			return errors.Join(writeErr, io.ErrShortWrite, truncateErr)
		}
		if _, seekErr := w.file.Seek(before, io.SeekStart); seekErr != nil {
			return errors.Join(writeErr, io.ErrShortWrite, seekErr)
		}
		if writeErr != nil {
			return writeErr
		}
		return io.ErrShortWrite
	}
	_, _ = w.digest.Write(encoded)
	w.written += int64(written)
	w.samples++
	return nil
}

func (w *artifactWriter) finalize(started, finished time.Time, duration, interval time.Duration, reason string, truncated bool) (Manifest, error) {
	manifest := Manifest{
		SchemaVersion: SchemaVersion, Kind: "brezel_host_telemetry", StartedAt: started.Format(time.RFC3339Nano), FinishedAt: finished.Format(time.RFC3339Nano),
		DurationNS: duration.Nanoseconds(), IntervalNS: interval.Nanoseconds(), SampleCount: w.samples, SampleBytes: w.written,
		MaxBytes: w.maxBytes, SamplesFile: samplesFilename, SamplesSHA256: hex.EncodeToString(w.digest.Sum(nil)), StopReason: reason, Truncated: truncated,
	}
	if err := w.file.Sync(); err != nil {
		_ = w.file.Close()
		return manifest, fmt.Errorf("sync telemetry samples: %w", err)
	}
	if err := w.file.Close(); err != nil {
		return manifest, fmt.Errorf("close telemetry samples: %w", err)
	}
	partPath := w.directory + string(os.PathSeparator) + samplesFilename + ".part"
	finalPath := w.directory + string(os.PathSeparator) + samplesFilename
	if err := os.Rename(partPath, finalPath); err != nil {
		return manifest, fmt.Errorf("commit telemetry samples: %w", err)
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return manifest, err
	}
	manifestBytes = append(manifestBytes, '\n')
	if err := writeDurableFile(w.directory, manifestFilename, manifestBytes); err != nil {
		return manifest, err
	}
	manifestDigest := sha256.Sum256(manifestBytes)
	checksums := []byte(manifest.SamplesSHA256 + "  " + samplesFilename + "\n" + hex.EncodeToString(manifestDigest[:]) + "  " + manifestFilename + "\n")
	if err := writeDurableFile(w.directory, checksumsFilename, checksums); err != nil {
		return manifest, err
	}
	if err := syncDirectory(w.directory); err != nil {
		return manifest, fmt.Errorf("sync telemetry directory: %w", err)
	}
	return manifest, nil
}

func writeDurableFile(directory, name string, data []byte) (resultErr error) {
	partPath := directory + string(os.PathSeparator) + "." + name + ".part"
	finalPath := directory + string(os.PathSeparator) + name
	file, err := os.OpenFile(partPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if resultErr != nil {
			_ = file.Close()
			_ = os.Remove(partPath)
		}
	}()
	if written, err := file.Write(data); err != nil {
		return err
	} else if written != len(data) {
		return io.ErrShortWrite
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(partPath, finalPath); err != nil {
		return err
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
