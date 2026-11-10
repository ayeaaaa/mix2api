//go:build !windows

package update

import (
	"os"
	"syscall"
)

func secureOpenFlags() int {
	return syscall.O_CLOEXEC | syscall.O_NOFOLLOW
}

func fileOwner(info os.FileInfo) (uint32, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return 0, false
	}
	return stat.Uid, true
}
