//go:build !unix

package undone

import "errors"

// lockFile is not implemented on this platform; the store falls back to its
// in-process mutex.
func lockFile(string) (func(), error) {
	return nil, errors.New("advisory file locks are not supported on this platform")
}
