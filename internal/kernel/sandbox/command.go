package sandbox

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"sync"
	"time"
)

var errEngineOutputLimit = errors.New("OCI engine output exceeded limit")

func runBoundedEngineCommand(
	ctx context.Context,
	executable string,
	maxOutputBytes int,
	arguments ...string,
) ([]byte, error) {
	commandCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(commandCtx, executable, arguments...)
	command.WaitDelay = time.Second
	output := &boundedEngineOutput{
		limit:  maxOutputBytes,
		cancel: cancel,
	}
	command.Stdout = output
	command.Stderr = output
	err := command.Run()
	if output.limitExceeded() {
		return output.bytes(), errEngineOutputLimit
	}
	return output.bytes(), err
}

type boundedEngineOutput struct {
	mu       sync.Mutex
	buffer   bytes.Buffer
	limit    int
	exceeded bool
	cancel   context.CancelFunc
}

func (output *boundedEngineOutput) Write(data []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	if output.exceeded {
		return len(data), nil
	}
	remaining := output.limit - output.buffer.Len()
	if remaining < len(data) {
		if remaining > 0 {
			_, _ = output.buffer.Write(data[:remaining])
		}
		output.exceeded = true
		output.cancel()
		return len(data), nil
	}
	_, _ = output.buffer.Write(data)
	return len(data), nil
}

func (output *boundedEngineOutput) bytes() []byte {
	output.mu.Lock()
	defer output.mu.Unlock()
	return append([]byte(nil), output.buffer.Bytes()...)
}

func (output *boundedEngineOutput) limitExceeded() bool {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.exceeded
}
