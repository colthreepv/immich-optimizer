//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package jobqueue

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
)

func lockProcess(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %v", ErrLocked, err)
	}
	return f, nil
}
func unlockProcess(f *os.File) error      { return f.Close() }
func atomicReplace(from, to string) error { return os.Rename(from, to) }
func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
