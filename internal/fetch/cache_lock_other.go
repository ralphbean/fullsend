//go:build !unix

package fetch

// lockCacheEntry is a no-op where flock is unavailable; fullsend's
// release builds are unix-only.
func lockCacheEntry(string) (func(), error) { return func() {}, nil }
