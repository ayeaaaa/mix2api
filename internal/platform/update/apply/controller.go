package apply

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/update"
)

// CommandSpec is one executable and its fixed argument list. It deliberately
// has no shell parsing: operators who need shell syntax should point Path at a
// reviewed wrapper script.
type CommandSpec struct {
	Path string
	Args []string
}

// Empty reports whether the command is unset.
func (c CommandSpec) Empty() bool { return strings.TrimSpace(c.Path) == "" }

// CommandController runs operator-provided stop/start commands. The fixed
// arguments are never derived from the request document.
type CommandController struct {
	StopCommand  CommandSpec
	StartCommand CommandSpec
	Timeout      time.Duration
	Logger       func(string)
}

// Stop executes the stop hook, if configured.
func (c *CommandController) Stop(ctx context.Context, request update.Request) error {
	return c.run(ctx, c.StopCommand, request, "stop")
}

// Start executes the start hook, if configured.
func (c *CommandController) Start(ctx context.Context, request update.Request) error {
	return c.run(ctx, c.StartCommand, request, "start")
}

func (c *CommandController) run(ctx context.Context, spec CommandSpec, request update.Request, phase string) error {
	if spec.Empty() {
		return nil
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	program, args := commandProgram(spec, runtime.GOOS)
	command := exec.CommandContext(runCtx, program, args...)
	configureBatchCommand(command, spec)
	command.Dir = request.Root
	command.Env = append(baseEnv(), "MIX2API_UPDATE_PHASE="+phase,
		fmt.Sprintf("MIX2API_UPDATE_PORT=%d", request.Port),
		"MIX2API_UPDATE_VERSION="+request.Version)
	output := &limitedBuffer{limit: 8 << 10}
	command.Stdout = output
	command.Stderr = output
	err := command.Run()
	if c.Logger != nil {
		if text := strings.TrimSpace(output.String()); text != "" {
			c.Logger("update: " + phase + " command: " + truncate(text, 400))
		}
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("update: %s command timed out after %s", phase, timeout)
	}
	if err != nil {
		return fmt.Errorf("update: %s command failed: %w: %s", phase, err, truncate(strings.TrimSpace(output.String()), 240))
	}
	return nil
}

// commandProgram keeps Windows batch hooks safe: CreateProcess cannot execute
// a .cmd/.bat directly, so it must run through cmd.exe /d /s /c. Executables
// and other platforms keep the fixed executable + argument form (no shell).
func commandProgram(spec CommandSpec, goos string) (string, []string) {
	if goos == "windows" {
		switch strings.ToLower(filepath.Ext(spec.Path)) {
		case ".cmd", ".bat":
			// The whole command is one argument here; Windows uses
			// batchCommandLine through SysProcAttr so Go's argv quoting cannot
			// split a path containing spaces.
			return "cmd.exe", []string{"/d", "/s", "/c", batchCommandLine(spec)}
		}
	}
	return spec.Path, append([]string(nil), spec.Args...)
}

// batchCommandLine builds the canonical cmd.exe command line. cmd /s strips
// the outer pair of quotes, leaving each path/argument quoted exactly once:
//
//	cmd.exe /d /s /c ""C:\Program Files\mix2api\restart.cmd" "--fast""
func batchCommandLine(spec CommandSpec) string {
	parts := []string{quoteCmdArgument(spec.Path)}
	for _, arg := range spec.Args {
		parts = append(parts, quoteCmdArgument(arg))
	}
	return `cmd.exe /d /s /c "` + strings.Join(parts, " ") + `"`
}

func quoteCmdArgument(value string) string {
	// Windows paths cannot contain a quote, but fixed operator args may; cmd
	// represents an embedded quote by doubling it.
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func baseEnv() []string {
	return append([]string(nil), os.Environ()...)
}

type limitedBuffer struct {
	mu    sync.Mutex
	data  []byte
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.limit - len(b.data)
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		b.data = append(b.data, p...)
	}
	return original, nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}
