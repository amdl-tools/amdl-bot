//go:build windows

package bot

import (
	"os/exec"
	"strconv"
)

// setProcessGroup is a no-op on Windows; killProcessTree walks the tree instead.
func setProcessGroup(cmd *exec.Cmd) {}

// killProcessTree kills the command and every process it started.
func killProcessTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	// taskkill /T ends the process together with its children.
	err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
	if err != nil {
		_ = cmd.Process.Kill()
	}
}
