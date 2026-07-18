//go:build linux

package tray

import (
	"fmt"
	"syscall"
	"testing"
)

func TestIsLockHeldError(t *testing.T) {
	if !isLockHeldError(syscall.EWOULDBLOCK) {
		t.Fatal("EWOULDBLOCK should be treated as an existing instance")
	}
	if !isLockHeldError(fmt.Errorf("wrapped: %w", syscall.EAGAIN)) {
		t.Fatal("wrapped EAGAIN should be treated as an existing instance")
	}
	if isLockHeldError(syscall.EINTR) {
		t.Fatal("EINTR should be reported as an unexpected lock error")
	}
}
