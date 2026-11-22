//go:build !unix

package processor

import (
	"os/exec"
	"time"
)

func configureCmd(cmd *exec.Cmd) {
	cmd.WaitDelay = 5 * time.Second
}

func killGroup(*exec.Cmd) {}
