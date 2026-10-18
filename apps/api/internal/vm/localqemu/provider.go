package localqemu

import (
	"context"
	"fmt"
	"os/exec"
)

type Config struct {
	QEMUBin string

	Options
}

type Provider struct {
	qemuBin string
	opts    Options
	command func(ctx context.Context, name string, args ...string) *exec.Cmd
}

func New(cfg Config) (*Provider, error) {
	opts, err := cfg.Options.normalized()
	if err != nil {
		return nil, err
	}

	bin := cfg.QEMUBin
	if bin == "" {
		bin = "qemu-system-x86_64"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("localqemu: QEMU not found: %w", err)
	}

	return &Provider{
		qemuBin: path,
		opts:    opts,
		command: exec.CommandContext,
	}, nil
}
