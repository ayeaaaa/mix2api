//go:build windows

package apply

import (
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// configureBatchCommand supplies a raw CreateProcess command line for batch
// hooks. os/exec escapes every argv element for the CRT, which would prefix
// quoted paths with backslashes that cmd.exe does not treat as escapes; a raw
// /d /s /c line keeps a path containing spaces intact.
func configureBatchCommand(command *exec.Cmd, spec CommandSpec) {
	if command == nil {
		return
	}
	switch strings.ToLower(filepath.Ext(spec.Path)) {
	case ".cmd", ".bat":
		if command.SysProcAttr == nil {
			command.SysProcAttr = &syscall.SysProcAttr{}
		}
		command.SysProcAttr.CmdLine = batchCommandLine(spec)
	}
}
