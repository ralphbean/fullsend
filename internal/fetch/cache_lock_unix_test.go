//go:build unix

package fetch

import (
	"strings"
	"testing"
	"time"
)

func TestLockCacheEntry_GivesUpOnAStuckHolder(t *testing.T) {
	dir := t.TempDir()
	unlock, err := lockCacheEntry(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	saved := cacheLockWait
	cacheLockWait = 200 * time.Millisecond
	defer func() { cacheLockWait = saved }()

	start := time.Now()
	_, err = lockCacheEntry(dir)
	if err == nil {
		t.Fatal("second lock succeeded while the first was held")
	}
	if !strings.Contains(err.Error(), "has held the cache lock") {
		t.Errorf("unexpected error: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("waited %s, want about %s", time.Since(start), cacheLockWait)
	}
}

func TestLockCacheEntry_WaitsForARelease(t *testing.T) {
	dir := t.TempDir()
	unlock, err := lockCacheEntry(dir)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		unlock()
	}()
	unlock2, err := lockCacheEntry(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	unlock2()
}
