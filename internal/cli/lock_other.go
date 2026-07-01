//go:build windows

package cli

// withCredentialsLock runs fn without cross-process locking on platforms that lack flock. The
// fallback for a concurrent refresh against a rotating provider is a rare extra `loft login`, not
// data loss.
func withCredentialsLock(fn func() error) error {
	return fn()
}
