//go:build unix

package layout

import (
	"os"
	"syscall"
)

func ownedByRoot(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Uid == 0
}
