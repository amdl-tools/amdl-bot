//go:build !windows

package bot

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the command in a process group of its own, so that it and
// everything it starts (ffmpeg, ...) can be stopped together.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessTree kills the command and every process in its group.
func killProcessTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	// A negative PID addresses the whole process group.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}
