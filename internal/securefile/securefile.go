// Package securefile reads operator-provisioned credentials and trust material
// without following a final-component symlink or accepting an ambiguously
// replaced file.
//
// A configured path is an operator-selected location: relative paths and
// equivalent spellings such as "./secrets/token" are supported. Read rejects
// empty or control-character-bearing path strings, but it does not attempt to
// confine a path containing ".." to a directory. Callers that need confinement
// must enforce that boundary before calling Read.
package securefile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Read returns at most maximum bytes from a private regular file owned by the
// effective process user. The file must have exactly one hard link. Read checks
// the path before open, the opened descriptor, and the path again after reading
// so a replacement is never silently accepted.
func Read(path string, maximum int64) ([]byte, error) {
	return read(path, maximum, nil)
}

// ReadText reads one non-empty logical line. Surrounding whitespace, including
// one trailing newline, is removed; an embedded CR or LF is rejected.
func ReadText(path string, maximum int64) (string, error) {
	data, err := Read(path, maximum)
	if err != nil {
		return "", err
	}
	defer wipe(data)
	value := strings.TrimSpace(string(data))
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return "", errors.New("protected file must contain exactly one non-empty line")
	}
	return value, nil
}

func read(path string, maximum int64, afterOpen func()) ([]byte, error) {
	if maximum < 1 {
		return nil, errors.New("protected file requires a positive size limit")
	}
	if err := validatePath(path); err != nil {
		return nil, err
	}
	path = filepath.Clean(path)

	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err := validateInfo(before); err != nil {
		return nil, err
	}

	file, err := openNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if err := validateInfo(opened); err != nil {
		return nil, err
	}
	if !os.SameFile(before, opened) {
		return nil, errors.New("protected file changed while opening")
	}

	if afterOpen != nil {
		afterOpen()
	}
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		wipe(data)
		return nil, err
	}
	after, err := os.Lstat(path)
	if err != nil {
		wipe(data)
		return nil, errors.New("protected file changed while reading")
	}
	if err := validateInfo(after); err != nil {
		wipe(data)
		return nil, err
	}
	if !os.SameFile(opened, after) {
		wipe(data)
		return nil, errors.New("protected file changed while reading")
	}
	if int64(len(data)) > maximum {
		wipe(data)
		return nil, fmt.Errorf("protected file exceeds %d bytes", maximum)
	}
	if len(data) == 0 {
		return nil, errors.New("protected file cannot be empty")
	}
	return data, nil
}

func wipe(data []byte) {
	for index := range data {
		data[index] = 0
	}
}

func validatePath(path string) error {
	if path == "" || strings.TrimSpace(path) != path {
		return errors.New("protected file path is required without surrounding whitespace")
	}
	for _, character := range []byte(path) {
		if character < 0x20 || character == 0x7f {
			return errors.New("protected file path cannot contain control characters")
		}
	}
	return nil
}

func validateInfo(info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("protected path must name a regular file, not a symlink")
	}
	permissions := info.Mode().Perm()
	if permissions&0o077 != 0 || permissions&0o400 == 0 || permissions&0o100 != 0 {
		return errors.New("protected file permissions must be 0400 or 0600")
	}
	return validateOwnerAndLinks(info)
}
