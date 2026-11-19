//go:build unix

package undone

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive advisory lock on path (creating it), blocking
// until it is available. The returned function releases it.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o666) // #nosec G302 G304 -- lock file at the undone root
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX) // #nosec G115 -- fd fits in int
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) // #nosec G115
		_ = f.Close()
	}, nil
}
