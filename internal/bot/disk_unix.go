//go:build !windows

package bot

import "syscall"

// diskUsagePct returns the percentage of disk space used on the filesystem
// containing path (0-100). This covers Linux, Android, macOS and the BSDs.
func diskUsagePct(path string) (float64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	total := uint64(stat.Blocks) * uint64(stat.Bsize)
	free := uint64(stat.Bfree) * uint64(stat.Bsize)
	if total == 0 {
		return 0, nil
	}
	return float64(total-free) / float64(total) * 100, nil
}
