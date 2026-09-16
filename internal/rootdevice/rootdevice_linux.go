//go:build linux

package rootdevice

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const fsImmutableFlag = 0x00000010

type Store struct {
	root      string
	rootfd    int
	sourcefd  int
	sandboxfd int
}

func openRoot(root string) (int, error) {
	if _, err := ValidateRootPath(root); err != nil {
		return -1, err
	}
	how := &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW,
		Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	}
	fd, err := unix.Openat2(unix.AT_FDCWD, root, how)
	if err != nil {
		return -1, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		unix.Close(fd)
		return -1, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mode&0o077 != 0 || stat.Uid != uint32(os.Geteuid()) {
		unix.Close(fd)
		return -1, errors.New("qualification root must be a private directory owned by the effective user")
	}
	return fd, nil
}

func openBeneath(dirfd int, name string, flags int, mode uint32) (int, error) {
	how := &unix.OpenHow{Flags: uint64(flags | unix.O_CLOEXEC | unix.O_NOFOLLOW), Mode: uint64(mode), Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS}
	return unix.Openat2(dirfd, name, how)
}

func ensureSandboxDir(rootfd int) (int, error) {
	err := unix.Mkdirat(rootfd, SandboxesName, 0o700)
	if err != nil && !errors.Is(err, unix.EEXIST) {
		return -1, err
	}
	fd, err := openBeneath(rootfd, SandboxesName, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return -1, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		unix.Close(fd)
		return -1, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mode&0o077 != 0 || stat.Uid != uint32(os.Geteuid()) {
		unix.Close(fd)
		return -1, errors.New("sandbox root must be a private owned directory")
	}
	return fd, nil
}

func fsType(magic int64) string {
	switch uint64(magic) {
	case unix.XFS_SUPER_MAGIC:
		return "xfs"
	case unix.BTRFS_SUPER_MAGIC:
		return "btrfs"
	default:
		return "unknown"
	}
}

func Inspect(root string) (Filesystem, error) {
	rootfd, err := openRoot(root)
	if err != nil {
		return Filesystem{}, err
	}
	defer unix.Close(rootfd)
	var statfs unix.Statfs_t
	if err := unix.Fstatfs(rootfd, &statfs); err != nil {
		return Filesystem{}, err
	}
	result := Filesystem{Type: fsType(int64(statfs.Type)), Magic: uint64(statfs.Type)}
	if result.Type == "unknown" {
		return result, errors.New("qualification requires XFS or Btrfs")
	}
	started := time.Now()
	suffix, err := randomSuffix()
	if err != nil {
		return result, err
	}
	sourceName := fmt.Sprintf(".reflink-source-%d-%s", os.Getpid(), suffix)
	destinationName := fmt.Sprintf(".reflink-destination-%d-%s", os.Getpid(), suffix)
	source, err := openBeneath(rootfd, sourceName, unix.O_CREAT|unix.O_EXCL|unix.O_RDWR, 0o600)
	if err != nil {
		return result, err
	}
	destination := -1
	defer func() {
		if destination >= 0 {
			_ = unix.Close(destination)
			_ = unix.Unlinkat(rootfd, destinationName, 0)
		}
		_ = unix.Close(source)
		_ = unix.Unlinkat(rootfd, sourceName, 0)
		_ = unix.Fsync(rootfd)
	}()
	probe := []byte("brezel-reflink-probe")
	if count, err := unix.Write(source, probe); err != nil || count != len(probe) {
		if err == nil {
			err = io.ErrShortWrite
		}
		return result, err
	}
	if err := unix.Fsync(source); err != nil {
		return result, err
	}
	destination, err = openBeneath(rootfd, destinationName, unix.O_CREAT|unix.O_EXCL|unix.O_RDWR, 0o600)
	if err != nil {
		return result, err
	}
	if err := unix.IoctlFileClone(destination, source); err != nil {
		return result, fmt.Errorf("FICLONE probe: %w", err)
	}
	if err := unix.Fsync(destination); err != nil {
		return result, err
	}
	result.ReflinkCapable = true
	result.ProbeDurationNS = time.Since(started).Nanoseconds()
	return result, nil
}

func validateBaseFD(rootfd int, expectedDigest string) (Base, int, error) {
	if err := validateDigest(expectedDigest); err != nil {
		return Base{}, -1, err
	}
	fd, err := openBeneath(rootfd, BaseName, unix.O_RDONLY, 0)
	if err != nil {
		return Base{}, -1, err
	}
	fail := func(err error) (Base, int, error) { unix.Close(fd); return Base{}, -1, err }
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fail(err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o222 != 0 || stat.Size <= 0 {
		return fail(errors.New("base must be a non-empty, owned, single-link, non-writable regular file"))
	}
	flags, err := unix.IoctlGetInt(fd, unix.FS_IOC_GETFLAGS)
	if err != nil {
		return fail(fmt.Errorf("read base inode flags: %w", err))
	}
	if flags&fsImmutableFlag == 0 {
		return fail(errors.New("base inode must have the filesystem immutable flag"))
	}
	hash := sha256.New()
	buffer := make([]byte, 1<<20)
	var size int64
	for {
		count, readErr := unix.Read(fd, buffer)
		if count > 0 {
			_, _ = hash.Write(buffer[:count])
			size += int64(count)
		}
		if errors.Is(readErr, io.EOF) || (readErr == nil && count == 0) {
			break
		}
		if readErr != nil {
			return fail(readErr)
		}
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if digest != expectedDigest || size != stat.Size {
		return fail(errors.New("base digest or size does not match the immutable manifest"))
	}
	if _, err := unix.Seek(fd, 0, io.SeekStart); err != nil {
		return fail(err)
	}
	return Base{Path: BaseName, Size: size, SHA256: digest, ImmutableFlag: true}, fd, nil
}

func ValidateBase(root, expectedDigest string) (Base, error) {
	rootfd, err := openRoot(root)
	if err != nil {
		return Base{}, err
	}
	defer unix.Close(rootfd)
	base, fd, err := validateBaseFD(rootfd, expectedDigest)
	if fd >= 0 {
		unix.Close(fd)
	}
	return base, err
}

func OpenStore(root, expectedDigest string) (*Store, Base, error) {
	rootfd, err := openRoot(root)
	if err != nil {
		return nil, Base{}, err
	}
	base, sourcefd, err := validateBaseFD(rootfd, expectedDigest)
	if err != nil {
		unix.Close(rootfd)
		return nil, Base{}, err
	}
	sandboxfd, err := ensureSandboxDir(rootfd)
	if err != nil {
		unix.Close(sourcefd)
		unix.Close(rootfd)
		return nil, Base{}, err
	}
	return &Store{root: root, rootfd: rootfd, sourcefd: sourcefd, sandboxfd: sandboxfd}, base, nil
}

func (store *Store) Close() error {
	if store == nil {
		return nil
	}
	first := unix.Close(store.sandboxfd)
	second := unix.Close(store.sourcefd)
	third := unix.Close(store.rootfd)
	if first != nil {
		return first
	}
	if second != nil {
		return second
	}
	return third
}

func randomSuffix() (string, error) {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func Clone(root, expectedDigest, sandboxID string) (CloneResult, error) {
	store, _, err := OpenStore(root, expectedDigest)
	if err != nil {
		return CloneResult{}, err
	}
	defer store.Close()
	return store.Clone(sandboxID)
}

func (store *Store) Clone(sandboxID string) (CloneResult, error) {
	if store == nil {
		return CloneResult{}, errors.New("root-device store is nil")
	}
	if err := ValidateSandboxID(sandboxID); err != nil {
		return CloneResult{}, err
	}
	started := time.Now()
	suffix, err := randomSuffix()
	if err != nil {
		return CloneResult{}, err
	}
	temporary := ".clone-" + sandboxID + "-" + suffix
	final := sandboxID + ".img"
	destination, err := openBeneath(store.sandboxfd, temporary, unix.O_CREAT|unix.O_EXCL|unix.O_RDWR, 0o600)
	if err != nil {
		return CloneResult{}, err
	}
	committed := false
	defer func() {
		unix.Close(destination)
		if !committed {
			_ = unix.Unlinkat(store.sandboxfd, temporary, 0)
			_ = unix.Fsync(store.sandboxfd)
		}
	}()
	if err := unix.IoctlFileClone(destination, store.sourcefd); err != nil {
		return CloneResult{}, fmt.Errorf("FICLONE: %w", err)
	}
	if err := unix.Fsync(destination); err != nil {
		return CloneResult{}, err
	}
	if err := unix.Renameat2(store.sandboxfd, temporary, store.sandboxfd, final, unix.RENAME_NOREPLACE); err != nil {
		return CloneResult{}, err
	}
	committed = true
	if err := unix.Fsync(store.sandboxfd); err != nil {
		return CloneResult{}, err
	}
	return CloneResult{Path: filepath.Join(store.root, SandboxesName, final), DurationNS: time.Since(started).Nanoseconds()}, nil
}

func Delete(root, sandboxID string) error {
	if err := ValidateSandboxID(sandboxID); err != nil {
		return err
	}
	rootfd, err := openRoot(root)
	if err != nil {
		return err
	}
	defer unix.Close(rootfd)
	sandboxfd, err := ensureSandboxDir(rootfd)
	if err != nil {
		return err
	}
	defer unix.Close(sandboxfd)
	err = unix.Unlinkat(sandboxfd, sandboxID+".img", 0)
	if err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return unix.Fsync(sandboxfd)
}

func Recover(root string) (int, error) {
	rootfd, err := openRoot(root)
	if err != nil {
		return 0, err
	}
	defer unix.Close(rootfd)
	sandboxfd, err := ensureSandboxDir(rootfd)
	if err != nil {
		return 0, err
	}
	defer unix.Close(sandboxfd)
	// directory owns a duplicate so the original descriptor remains valid.
	duplicate, err := unix.Dup(sandboxfd)
	if err != nil {
		return 0, err
	}
	directory := os.NewFile(uintptr(duplicate), SandboxesName)
	if directory == nil {
		unix.Close(duplicate)
		return 0, errors.New("open sandbox directory descriptor")
	}
	defer directory.Close()
	names, err := directory.Readdirnames(-1)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, name := range names {
		if !strings.HasPrefix(name, ".clone-") {
			continue
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(sandboxfd, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return removed, err
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFREG {
			return removed, errors.New("unsafe non-regular stale clone entry")
		}
		if err := unix.Unlinkat(sandboxfd, name, 0); err != nil {
			return removed, err
		}
		removed++
	}
	if removed > 0 {
		return removed, unix.Fsync(sandboxfd)
	}
	return 0, nil
}

func SetBaseImmutable(root string, immutable bool) error {
	rootfd, err := openRoot(root)
	if err != nil {
		return err
	}
	defer unix.Close(rootfd)
	fd, err := openBeneath(rootfd, BaseName, unix.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	flags, err := unix.IoctlGetInt(fd, unix.FS_IOC_GETFLAGS)
	if err != nil {
		return err
	}
	if immutable {
		flags |= fsImmutableFlag
	} else {
		flags &^= fsImmutableFlag
	}
	return unix.IoctlSetPointerInt(fd, unix.FS_IOC_SETFLAGS, flags)
}

func DedicatedMount(root string) error {
	rootfd, err := openRoot(root)
	if err != nil {
		return err
	}
	defer unix.Close(rootfd)
	var current, parent unix.Stat_t
	if err := unix.Fstat(rootfd, &current); err != nil {
		return err
	}
	parentHow := &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW,
		Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	}
	parentfd, err := unix.Openat2(unix.AT_FDCWD, filepath.Dir(root), parentHow)
	if err != nil {
		return err
	}
	defer unix.Close(parentfd)
	if err := unix.Fstat(parentfd, &parent); err != nil {
		return err
	}
	if current.Dev == parent.Dev {
		return errors.New("disk-full qualification requires a dedicated mountpoint")
	}
	marker, err := openBeneath(rootfd, DiskFullMarker, unix.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer unix.Close(marker)
	var stat unix.Stat_t
	if err := unix.Fstat(marker, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o077 != 0 {
		return errors.New("disk-full marker must be a private, owned, single-link regular file")
	}
	data := make([]byte, len(markerContents)+1)
	count := 0
	for count < len(data) {
		read, readErr := unix.Read(marker, data[count:])
		count += read
		if readErr != nil {
			return readErr
		}
		if read == 0 {
			break
		}
	}
	if count != len(markerContents) || string(data[:count]) != markerContents {
		return errors.New("disk-full marker has invalid contents")
	}
	return nil
}

// ExerciseDiskFull is intentionally restricted to an explicitly marked,
// dedicated mount. It fills only that filesystem, verifies that clone/COW
// failure leaves the immutable base intact, and removes all created files.
func ExerciseDiskFull(root, expectedDigest, sandboxID string) (resultErr error) {
	if err := DedicatedMount(root); err != nil {
		return err
	}
	if err := ValidateSandboxID(sandboxID); err != nil {
		return err
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(root, &fs); err != nil {
		return err
	}
	fillerPath := filepath.Join(root, ".disk-full-filler")
	filler, err := os.OpenFile(fillerPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		_ = filler.Close()
		_ = os.Remove(fillerPath)
		_ = Delete(root, sandboxID)
		_, _ = Recover(root)
		if syncErr := syncParent(fillerPath); resultErr == nil && syncErr != nil {
			resultErr = syncErr
		}
	}()
	chunk := make([]byte, 4<<20)
	maximumWrites := int64(fs.Blocks)*int64(fs.Bsize)/int64(len(chunk)) + 2
	full := false
	for index := int64(0); index < maximumWrites; index++ {
		_, writeErr := filler.Write(chunk)
		if errors.Is(writeErr, unix.ENOSPC) {
			full = true
			break
		}
		if writeErr != nil {
			return writeErr
		}
	}
	if !full {
		return errors.New("dedicated filesystem did not report ENOSPC")
	}
	_ = filler.Sync() // ENOSPC is expected after exhausting the dedicated mount.
	clone, cloneErr := Clone(root, expectedDigest, sandboxID)
	if cloneErr == nil {
		file, openErr := os.OpenFile(clone.Path, os.O_WRONLY, 0)
		if openErr != nil {
			return openErr
		}
		base, baseErr := ValidateBase(root, expectedDigest)
		if baseErr != nil {
			file.Close()
			return baseErr
		}
		cowFull := false
		for offset := int64(0); offset < base.Size; offset += int64(len(chunk)) {
			_, writeErr := file.WriteAt(chunk, offset)
			if errors.Is(writeErr, unix.ENOSPC) {
				cowFull = true
				break
			}
			if writeErr != nil {
				file.Close()
				return writeErr
			}
		}
		_ = file.Close()
		if !cowFull {
			return errors.New("copy-on-write path did not report ENOSPC")
		}
	} else if !errors.Is(cloneErr, unix.ENOSPC) {
		return cloneErr
	}
	if _, err := ValidateBase(root, expectedDigest); err != nil {
		return fmt.Errorf("base changed during disk-full test: %w", err)
	}
	return nil
}
