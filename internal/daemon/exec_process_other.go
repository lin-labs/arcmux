//go:build !darwin && !linux

package daemon

import (
	"os"
	"os/exec"
)

func configureExecProcess(_ *exec.Cmd) {}

func signalExecProcessTree(proc *os.Process, signal os.Signal) error {
	return proc.Signal(signal)
}

func execProcessTreeAlive(proc *os.Process) bool {
	// The fallback has no portable process-group probe. Re-sending Kill is
	// harmless after the owned process has already entered forced shutdown.
	return proc.Signal(os.Kill) == nil
}
