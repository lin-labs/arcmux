package hooks

import (
	"fmt"
	"os"
	"syscall"
)

// lockSessionState acquires an exclusive advisory lock for a session's state
// file via a sidecar "<path>.lock" held with flock(2). It returns an unlock
// func that releases the lock and closes the descriptor. Serializes concurrent
// `arcmux hook` read-modify-write cycles for the same session.
func lockSessionState(path string) (func(), error) {
	lockPath := path + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open session state lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("flock session state lock: %w", err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
