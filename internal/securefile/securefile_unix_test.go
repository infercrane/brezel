//go:build darwin || linux

package securefile

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

type fakeFileInfo struct{ stat *syscall.Stat_t }

func (fakeFileInfo) Name() string       { return "secret" }
func (fakeFileInfo) Size() int64        { return 1 }
func (fakeFileInfo) Mode() os.FileMode  { return 0o600 }
func (fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (fakeFileInfo) IsDir() bool        { return false }
func (info fakeFileInfo) Sys() any      { return info.stat }

func TestValidateOwnerAndLinksRejectsWrongOwnerAndHardlink(t *testing.T) {
	wrongOwner := uint32(os.Geteuid()) + 1
	if err := validateOwnerAndLinks(fakeFileInfo{stat: &syscall.Stat_t{Uid: wrongOwner, Nlink: 1}}); err == nil || !strings.Contains(err.Error(), "owned") {
		t.Fatalf("wrong owner error = %v", err)
	}
	if err := validateOwnerAndLinks(fakeFileInfo{stat: &syscall.Stat_t{Uid: uint32(os.Geteuid()), Nlink: 2}}); err == nil || !strings.Contains(err.Error(), "hard link") {
		t.Fatalf("hardlink error = %v", err)
	}
}
