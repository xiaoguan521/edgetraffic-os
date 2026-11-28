//go:build unix

package collect

import "syscall"

// DiskUsage 返回目录所在文件系统的空间与 inode 使用率（百分比）。
func DiskUsage(dir string) (usedPct, inodePct float64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	if st.Blocks > 0 {
		usedPct = float64(st.Blocks-st.Bavail) * 100 / float64(st.Blocks)
	}
	if st.Files > 0 {
		inodePct = float64(st.Files-st.Ffree) * 100 / float64(st.Files)
	}
	return usedPct, inodePct, nil
}
