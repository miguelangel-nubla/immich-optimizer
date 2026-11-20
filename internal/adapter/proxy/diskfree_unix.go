//go:build unix

package proxy

import (
	"math"
	"os"
	"syscall"
)

// tempFree returns the space left for this process in the temp folder.
func tempFree() (int64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(os.TempDir(), &st); err != nil {
		return 0, false
	}
	free := uint64(st.Bavail) * uint64(st.Bsize) // #nosec G115 -- both are non-negative counts
	if free > math.MaxInt64 {
		return math.MaxInt64, true
	}
	return int64(free), true
}
