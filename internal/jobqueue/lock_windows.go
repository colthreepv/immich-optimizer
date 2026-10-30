package jobqueue

import (
	"fmt"
	"golang.org/x/sys/windows"
	"os"
)

func lockProcess(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	var overlapped windows.Overlapped
	if err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %v", ErrLocked, err)
	}
	return f, nil
}
func unlockProcess(f *os.File) error { return f.Close() }
func atomicReplace(from, to string) error {
	f, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	t, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(f, t, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

// Windows does not expose a portable directory fsync. Flush file data before
// MoveFileEx's write-through rename to make the snapshot durable on NTFS.
func syncDirectory(path string) error { return nil }
