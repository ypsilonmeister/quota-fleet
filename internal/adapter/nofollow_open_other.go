//go:build !unix

package adapter

import "os"

// openNoFollow on platforms without O_NOFOLLOW: the caller's re-check and os.SameFile
// comparison after the open detect a link swapped in.
func openNoFollow(path string) (*os.File, error) {
	return os.Open(path)
}
