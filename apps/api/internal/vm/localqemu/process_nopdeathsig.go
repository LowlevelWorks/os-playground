//go:build unix && !linux

package localqemu

import "os/exec"

// setPdeathsig does nothing: only Linux can have the kernel kill a child when
// its parent dies. Elsewhere a crashed server can leave QEMU running.
func setPdeathsig(*exec.Cmd) {}
