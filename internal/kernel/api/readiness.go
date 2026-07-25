package api

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	readinessTimeout       = 2 * time.Second
	readinessCacheDuration = 5 * time.Second
	readinessOutputLimit   = 64 << 10
)

func (s *Server) dependenciesReady(ctx context.Context) error {
	s.readinessMu.Lock()
	defer s.readinessMu.Unlock()
	checkedAt := s.readinessCheckedAt
	if !checkedAt.IsZero() {
		age := s.now().Sub(checkedAt)
		if age >= 0 && age < readinessCacheDuration {
			return s.readinessErr
		}
	}

	err := s.store.Health(ctx)
	if err == nil {
		err = s.checkRuntime(ctx)
	}
	s.readinessCheckedAt = s.now()
	s.readinessErr = err
	return err
}

func (s *Server) checkRuntime(ctx context.Context) error {
	if strings.TrimSpace(s.executionPolicy.EnginePath) == "" {
		return nil
	}
	if err := runReadinessCommand(
		ctx,
		s.executionPolicy.EnginePath,
		"info",
		"--format",
		"{{.ServerVersion}}",
	); err != nil {
		return err
	}

	images := s.requiredRuntimeImages()
	if len(images) == 0 {
		return nil
	}
	arguments := []string{"image", "inspect", "--format", "{{.Id}}"}
	arguments = append(arguments, images...)
	return runReadinessCommand(ctx, s.executionPolicy.EnginePath, arguments...)
}

func (s *Server) requiredRuntimeImages() []string {
	images := make(map[string]struct{})
	if len(s.executionPolicy.AllowedAgentImages) > 0 {
		for _, image := range s.executionPolicy.AllowedAgentImages {
			if image = strings.TrimSpace(image); image != "" {
				images[image] = struct{}{}
			}
		}
	} else {
		allowed := s.executionPolicy.AllowedAgentImageDigests
		if allowed == nil {
			allowed = s.executionPolicy.AllowedImageDigests
		}
		for image := range allowed {
			images[image] = struct{}{}
		}
	}
	if profiles := s.executionPolicy.VerifierProfiles; profiles != nil {
		for _, profile := range profiles.Profiles() {
			images[profile.Image] = struct{}{}
		}
	}
	if broker := s.executionPolicy.ModelBroker; broker != nil {
		if image := strings.TrimSpace(broker.Image); image != "" {
			images[image] = struct{}{}
		}
	}
	result := make([]string, 0, len(images))
	for image := range images {
		result = append(result, image)
	}
	sort.Strings(result)
	return result
}

func runReadinessCommand(ctx context.Context, executable string, arguments ...string) error {
	commandCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	command := exec.CommandContext(commandCtx, executable, arguments...)
	output := &readinessBuffer{limit: readinessOutputLimit, cancel: cancel}
	command.Stdout = output
	command.Stderr = output
	if err := command.Run(); err != nil {
		if output.exceeded {
			return errors.New("runtime readiness command exceeded output limit")
		}
		return errors.New("runtime readiness command failed")
	}
	if output.exceeded {
		return errors.New("runtime readiness command exceeded output limit")
	}
	return nil
}

type readinessBuffer struct {
	mu       sync.Mutex
	buffer   bytes.Buffer
	limit    int
	exceeded bool
	cancel   context.CancelFunc
}

func (buffer *readinessBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	if buffer.exceeded {
		return len(data), nil
	}
	remaining := buffer.limit - buffer.buffer.Len()
	if remaining < len(data) {
		if remaining > 0 {
			_, _ = buffer.buffer.Write(data[:remaining])
		}
		buffer.exceeded = true
		buffer.cancel()
		return len(data), nil
	}
	_, _ = buffer.buffer.Write(data)
	return len(data), nil
}
