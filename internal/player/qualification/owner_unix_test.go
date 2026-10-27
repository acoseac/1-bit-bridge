//go:build !windows

package qualification

import (
	"os"
	"syscall"
	"testing"
)

type ownerInfo struct {
	os.FileInfo
	uid uint32
}

func (s ownerInfo) Sys() any { return &syscall.Stat_t{Uid: s.uid} }
func TestApprovalWrongOwner(t *testing.T) {
	st, e := os.Stat(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if !owned(ownerInfo{st, uint32(os.Geteuid())}) {
		t.Fatal("bridge owner refused")
	}
	if owned(ownerInfo{st, uint32(os.Geteuid() + 1)}) {
		t.Fatal("wrong owner trusted")
	}
}
