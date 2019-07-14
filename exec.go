package main

import (
	"context"
	"os/exec"

	"github.com/go-kit/kit/log"
)

func ExecCommand(ctx context.Context, logger log.Logger, command string) error {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	combinedOutput, err := cmd.CombinedOutput()
	logger.Log("output", string(combinedOutput))
	return err
}
