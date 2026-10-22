//go:build linux

package localqemu

import (
	"os/exec"
	"syscall"
)

// setPdeathsig makes the kernel send SIGKILL to the child if the
// parent dies, so a crashed server can't leave QEMU processes behind.
// Linux only — macOS has no Pdeathsig, so strays are possible there.
func setPdeathsig(cmd *exec.Cmd) {
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}
