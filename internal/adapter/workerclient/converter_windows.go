//go:build windows

package workerclient

import (
	"context"
	"os/exec"
	"strings"
)

func runConverter(ctx context.Context, program, source, output string) (string, error) {
	cmd := exec.CommandContext(ctx, program, source, output)
	b, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(b)), err
}
