//go:build !windows

package rootfs

import "syscall"

// notDirectoryErrnos is how Unix says "this exists, but it is not a directory".
var notDirectoryErrnos = []error{syscall.ENOTDIR}
