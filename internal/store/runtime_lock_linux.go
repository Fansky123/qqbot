//go:build linux

package store

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

var (
	ErrRuntimeLocked       = errors.New("runtime already locked")
	ErrRuntimeLockRequired = errors.New("held runtime lock required")
)

const runtimeLockSuffix = ".runtime.lock"

// RuntimeLock proves that one process exclusively owns recovery and scheduling.
type RuntimeLock struct {
	mu    sync.Mutex
	store *Store
	file  *os.File
}

func (s *Store) AcquireRuntimeLock() (*RuntimeLock, error) {
	fd, err := unix.Openat(s.dirFD, s.lockName, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open runtime lock: %w", err)
	}
	closeFD := func() { _ = unix.Close(fd) }
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		closeFD()
		return nil, fmt.Errorf("inspect runtime lock: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 || stat.Mode&0o7177 != 0 {
		closeFD()
		return nil, errors.New("unsafe runtime lock file")
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		closeFD()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrRuntimeLocked
		}
		return nil, fmt.Errorf("acquire runtime lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), s.path+runtimeLockSuffix)
	if file == nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		closeFD()
		return nil, errors.New("create runtime lock handle failed")
	}
	return &RuntimeLock{store: s, file: file}, nil
}

func (l *RuntimeLock) withStore(store *Store, fn func() error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil || l.store != store {
		return ErrRuntimeLockRequired
	}
	if err := unix.Flock(int(l.file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.Join(ErrRuntimeLockRequired, err)
	}
	return fn()
}

func (l *RuntimeLock) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	l.store = nil
	unlockErr := unix.Flock(int(file.Fd()), unix.LOCK_UN)
	closeErr := file.Close()
	if unlockErr != nil || closeErr != nil {
		return errors.Join(unlockErr, closeErr)
	}
	return nil
}
