package rootfs

import "syscall"

// errorDirectory is the Win32 ERROR_DIRECTORY ("the directory name is
// invalid"), which is what the NT layer's STATUS_NOT_A_DIRECTORY surfaces as
// when a directory open is attempted on a file. package syscall does not
// export it.
const errorDirectory = syscall.Errno(267)

// notDirectoryErrnos are the ways Windows says "this exists, but it is not a
// directory". Opening a root goes through the NT layer and reports
// ERROR_DIRECTORY; the Win32 calls used elsewhere report ENOTDIR.
var notDirectoryErrnos = []error{syscall.ENOTDIR, errorDirectory}
