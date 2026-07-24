//go:build windows

package auth

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
	"unsafe"
)

const (
	lockfileExclusiveLock = 0x00000002
	lockfileFailImmediate = 0x00000001
)

var (
	kernel32       = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx = kernel32.NewProc("LockFileEx")
	procUnlockFile = kernel32.NewProc("UnlockFileEx")
)

type authFileLock struct {
	file *os.File
	over syscall.Overlapped
}

func acquireAuthFileLock(ctx context.Context, authPath string) (*authFileLock, error) {
	path := authLockPath(authPath)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	_ = file.Chmod(0o600)
	lock := &authFileLock{file: file}
	for {
		result, _, callErr := procLockFileEx.Call(
			file.Fd(), lockfileExclusiveLock|lockfileFailImmediate, 0,
			1, 0, uintptr(unsafe.Pointer(&lock.over)),
		)
		if result != 0 {
			return lock, nil
		}
		if !errors.Is(callErr, syscall.Errno(33)) {
			file.Close()
			return nil, callErr
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (l *authFileLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	_, _, _ = procUnlockFile.Call(l.file.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&l.over)))
	err := l.file.Close()
	l.file = nil
	return err
}
