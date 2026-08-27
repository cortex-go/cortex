//go:build windows

package app

import "os/exec"

func configureProcessGroup(cmd *exec.Cmd) {}
