//go:build unix

package fetch

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockCacheEntry takes an exclusive lock on the cache entry directory dir,
// shared across processes, and returns the function that releases it.
// Writers replacing a materialized tree hold it, so one never removes a
// tree another has just installed.
func lockCacheEntry(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, ".tree.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening cache lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("acquiring cache lock: %w", err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
		f.Close()
	}, nil
}
