package jobs

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"syscall"
	"time"
)

// runStep runs one argv in its own process group. On cancellation it sends SIGINT to the
// group (terraform then releases state locks), and SIGKILL after grace.
func runStep(ctx context.Context, argv []string, dir string, env []string, log io.Writer, grace time.Duration) (int, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = log, log
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
