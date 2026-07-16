//go:build darwin || linux

package daemon

import (
	"os"
	"os/exec"
	"syscall"
)

func configureExecProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func signalExecProcessTree(proc *os.Process, signal os.Signal) error {
	sig, ok := signal.(syscall.Signal)
	if !ok {
		return proc.Signal(signal)
	}
	err := syscall.Kill(-proc.Pid, sig)
	if err == syscall.ESRCH {
		return nil
	}
	return err
}

func execProcessTreeAlive(proc *os.Process) bool {
	err := syscall.Kill(-proc.Pid, 0)
	return err == nil || err == syscall.EPERM
}
