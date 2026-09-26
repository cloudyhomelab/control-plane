package jobs

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/cloudyhome/controlplane/internal/tools"
)

// runStep runs one step in its own process group. On cancellation it sends SIGINT to the
// group (terraform then releases state locks), and SIGKILL after grace.
func runStep(ctx context.Context, step tools.Step, env []string, log io.Writer, grace time.Duration) (int, error) {
	cmd := exec.CommandContext(ctx, step.Argv[0], step.Argv[1:]...)
	cmd.Dir = step.Dir
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = log, log
	if step.StdoutFile != "" {
		stdoutFile, err := os.OpenFile(step.StdoutFile, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
		if err != nil {
			return -1, err
		}
		defer stdoutFile.Close()
		cmd.Stdout = stdoutFile
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGINT) }
	cmd.WaitDelay = grace

	err := cmd.Run()
	if ctx.Err() != nil && cmd.Process != nil {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), err
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}
