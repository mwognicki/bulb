package firewall

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) error
	RunInput(ctx context.Context, input string, name string, args ...string) error
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w: %s", name, args, err, bytes.TrimSpace(output))
	}
	return nil
}

func (ExecRunner) RunInput(ctx context.Context, input string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = bytes.NewBufferString(input)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w: %s", name, args, err, bytes.TrimSpace(output))
	}
	return nil
}
