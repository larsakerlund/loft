//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"syscall"
)

// withCredentialsLock serializes the read-refresh-write section across processes with an advisory
// exclusive flock on a sibling credentials.json.lock. Two parallel `loft deploy` runs can both hit a
// 401 and try to refresh; against a rotating provider the first spends the shared refresh token and
// the second would then see a spurious invalid_grant. The lock collapses them: the winner refreshes,
// the others re-read and adopt its result under the lock. flock is advisory and per-host, so on a
// network filesystem where it is a no-op the fallback is the pre-existing rare extra `loft login`.
func withCredentialsLock(fn func() error) error {
	p, err := credentialsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(p+".lock", os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- fixed path under the user's config dir
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()
	return fn()
}
