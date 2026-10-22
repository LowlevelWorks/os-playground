//go:build unix

package localqemu

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// setProcAttr puts the child in its own process group so that stopping
// a VM can kill the whole group (QEMU plus any children it spawned)
// instead of leaving strays. Lives here, not in process.go, so the
// fake QEMU used in tests gets it too.
func setProcAttr(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}

	cmd.SysProcAttr.Setpgid = true
}

// killGroup SIGKILLs the whole process group. Called by exec only while the
// process is alive, so the PID cannot have been reused

func killGroup(cmd *exec.Cmd) error {
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone //already gone: not a failure
	}
	return err
}
