# Reflink root-device qualification

This is an independent Linux qualification candidate. It does not select or
change Brezel's production root-device implementation.

The candidate requires Linux `openat2`, XFS with reflink enabled or Btrfs, and
permission to read and set inode flags. It validates an owned, single-link,
non-writable base image with both the filesystem immutable flag and a pinned
SHA-256 digest. Each sandbox image is created through `FICLONE` under a hidden
temporary name, fsynced, published with `RENAME_NOREPLACE`, and followed by a
parent-directory fsync. Delete is idempotent and directory-fsynced.

Path resolution is descriptor-relative with `RESOLVE_BENEATH`,
`RESOLVE_NO_SYMLINKS`, `RESOLVE_NO_MAGICLINKS`, and `O_NOFOLLOW`. Sandbox IDs
cannot contain separators or traversal. Crash recovery removes only regular
files carrying the private `.clone-` prefix and fails closed on a symlink or
other unexpected file type.

## Safe tests on any development host

```sh
go test -race ./internal/rootdevice ./cmd/brezel-rootdevice-qualify
GOOS=linux GOARCH=amd64 go test -c -o /dev/null ./internal/rootdevice
GOOS=linux GOARCH=amd64 go build -o /dev/null ./cmd/brezel-rootdevice-qualify
```

Non-Linux builds retain input-validation tests and return `ErrUnsupported` for
Linux filesystem operations.

## Privileged Linux qualification

Run only on a disposable Linux qualification host:

```sh
sudo make qualify-rootdevice-reflink
```

The script creates a new private work directory, attaches a new sparse 1 GiB
loop device, formats it as XFS with reflink enabled, mounts it with `nosuid` and
`nodev`, and removes the mount, loop device, and work directory on exit. The
disk-full test cannot run against `/`, an ordinary directory, or an unmarked
mount. It requires both a different device ID from the parent directory and an
exact private marker file created inside this disposable filesystem.

Qualification checks:

* actual filesystem type and a real `FICLONE` probe;
* immutable base mode, ownership, link count, inode flag, size, and digest;
* no-follow/no-replace behavior against a malicious final-component symlink;
* 64 clones across 16 workers with per-clone nanosecond timings;
* clone write isolation followed by base digest revalidation;
* simulated pre-commit crash residue and bounded recovery;
* idempotent deletion with directory fsync;
* real ENOSPC on the dedicated loop filesystem, including clone or COW failure,
  base revalidation, and cleanup.

The command prints one JSON object. `diagnostic_only` is always true; timings
are host-specific qualification evidence and are not a production or
cross-provider benchmark.

## Runtime activation

NBD remains the production default. The direct provider also retains the
packaged `/orchestrator/sandbox` cache default. Reflink is explicit opt-in and
requires two host paths on the same qualified XFS or Btrfs filesystem:

```sh
sudo install -d -m 0700 /brezel-rootfs/reflink-cache /brezel-rootfs/sandbox
export BREZEL_ENGINE_SANDBOX_ROOTFS_PROVIDER=reflink
export BREZEL_ENGINE_SANDBOX_ROOTFS_REFLINK_CACHE_DIR=/brezel-rootfs/reflink-cache
export BREZEL_ENGINE_SANDBOX_CACHE_DIR=/brezel-rootfs/sandbox
```

Both paths must already exist, be absolute private non-symlink directories,
and have the same filesystem device ID. The first holds verified immutable
base images. `BREZEL_ENGINE_SANDBOX_CACHE_DIR` holds the per-sandbox reflink
clones. The installer validates this boundary, including a self-cleaning live
`FICLONE` probe, before service mutation, and the runtime repeats the
same-filesystem and no-follow checks.
Neither cache may equal or sit below `SANDBOX_DIR` (`/fc-vm` by default):
Firecracker mounts tmpfs there in its private mount namespace, which would hide
the host rootfs source before it can be linked into the VM launch directory.
Selecting reflink does not establish a persistence, snapshot, or resume claim.
