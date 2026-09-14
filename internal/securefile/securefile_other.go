//go:build !darwin && !linux

package securefile

import (
	"errors"
	"os"
)

func openNoFollow(path string) (*os.File, error) {
	return os.Open(path)
}

func validateOwnerAndLinks(os.FileInfo) error {
	return errors.New("protected file ownership is unavailable on this platform")
}
