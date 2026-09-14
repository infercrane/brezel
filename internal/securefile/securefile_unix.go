//go:build darwin || linux

package securefile

import (
	"errors"
	"os"
	"syscall"
)

func openNoFollow(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, errors.New("open protected file descriptor")
	}
	return file, nil
}

func validateOwnerAndLinks(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("protected file ownership is unavailable")
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return errors.New("protected file must be owned by the current process user")
	}
	if stat.Nlink != 1 {
		return errors.New("protected file must have exactly one hard link")
	}
	return nil
}
