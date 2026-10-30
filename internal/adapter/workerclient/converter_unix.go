//go:build !windows

package workerclient

import (
	"bytes"
	"context"
	"os/exec"
	"syscall"
	"time"
)

func runConverter(ctx context.Context, program, source, output string) (string, error) {
	cmd := exec.Command(program, source, output)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stderr limitedBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return stderr.String(), err
	case <-ctx.Done():
		syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
		return stderr.String(), ctx.Err()
	}
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len() < 8192 {
		remaining := 8192 - b.Len()
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}
