package sandbox

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

func TestBoundedEngineCommandCancelsUnboundedOutput(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	started := time.Now()
	output, err := runBoundedEngineCommand(
		ctx,
		shell,
		1024,
		"-c",
		"while :; do printf '0123456789abcdef'; done",
	)
	if !errors.Is(err, errEngineOutputLimit) {
		t.Fatalf("error=%v, want output-limit error", err)
	}
	if len(output) != 1024 {
		t.Fatalf("captured output=%d bytes, want 1024", len(output))
	}
	if time.Since(started) >= 2*time.Second {
		t.Fatal("unbounded engine command was not cancelled promptly")
	}
}
