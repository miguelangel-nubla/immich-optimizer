//go:build unix

package processor

import (
	"os/exec"
	"syscall"
	"time"
)

// configureCmd runs the task in its own process group so that cancelling the
// context (task timeout or shutdown) kills the whole tree: `sh -c` and any
// grandchildren such as HandBrakeCLI or ffmpeg.
func configureCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second
}
