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
	Output(ctx context.Context, name string, args ...string) (string, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) error {
	_, err := runCombined(ctx, "", name, args...)
	return err
}

func (ExecRunner) RunInput(ctx context.Context, input string, name string, args ...string) error {
	_, err := runCombined(ctx, input, name, args...)
	return err
}

func (ExecRunner) Output(ctx context.Context, name string, args ...string) (string, error) {
	return runCombined(ctx, "", name, args...)
}

func runCombined(ctx context.Context, input string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if input != "" {
		cmd.Stdin = bytes.NewBufferString(input)
	}
	output, err := cmd.CombinedOutput()
	trimmed := string(bytes.TrimSpace(output))
	if err != nil {
		return trimmed, fmt.Errorf("%s %v: %w: %s", name, args, err, bytes.TrimSpace(output))
	}
	return trimmed, nil
}
