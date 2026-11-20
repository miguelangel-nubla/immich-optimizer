//go:build !unix

package proxy

// tempFree cannot tell on this system: uploads are never held back.
func tempFree() (int64, bool) { return 0, false }
