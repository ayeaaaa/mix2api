//go:build !windows

package apply

import "os/exec"

func configureBatchCommand(*exec.Cmd, CommandSpec) {}
