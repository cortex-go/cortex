//go:build !windows

package app

import (
	"os/exec"
	"syscall"
)

func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// processExitStatus extracts the process termination facts from an *exec.Cmd
// after Wait. The signal name is retained independently from the exit code.
func processExitStatus(cmd *exec.Cmd) exitStatus {
	ps := cmd.ProcessState
	if ps == nil {
		return exitStatus{}
	}
	out := exitStatus{exited: ps.Exited(), exitCode: ps.ExitCode()}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		out.signaled = true
		out.signal = ws.Signal().String()
	}
	return out
}
