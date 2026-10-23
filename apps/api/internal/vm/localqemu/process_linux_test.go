//go:build linux

package localqemu

import (
	"os/exec"
	"syscall"
	"testing"
)

func TestSetProcAttrAsksForPdeathsig(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "true")
	setProcAttr(cmd)
	if got := cmd.SysProcAttr.Pdeathsig; got != syscall.SIGKILL {
		t.Fatalf("Pdeathsig = %v, want SIGKILL", got)
	}
	if !cmd.SysProcAttr.Setpgid {
		t.Fatal("Setpgid was lost")
	}
}
