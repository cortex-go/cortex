//go:build windows

package app

import "os/exec"

func configureProcessGroup(cmd *exec.Cmd) {}

// processExitStatus extracts the process termination facts from an *exec.Cmd
// after Wait. On Windows there is no POSIX signal concept, so signaled stays
// false and the exit code is the process termination code.
func processExitStatus(cmd *exec.Cmd) exitStatus {
	ps := cmd.ProcessState
	if ps == nil {
		return exitStatus{}
	}
	return exitStatus{exited: ps.Exited(), exitCode: ps.ExitCode()}
}
