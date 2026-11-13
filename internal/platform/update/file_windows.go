//go:build windows

package update

import "os"

func secureOpenFlags() int { return 0 }

func fileOwner(os.FileInfo) (uint32, bool) { return 0, false }
