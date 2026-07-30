package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/model"
	"github.com/duriantaco/vouch/internal/kernel/runtimepreflight"
)

const runtimePreflightStageTimeout = 5 * time.Second

func (s *Server) preflightRuntime(w http.ResponseWriter, r *http.Request) {
	var request runtimepreflight.Request
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	if err := request.Validate(); err != nil {
		writeError(w, schemaError("invalid Runtime preflight request", err))
		return
	}
	resource := request.ExpectedRuntimeID
	if request.Agent != nil {
		resource = request.Agent.Profile.ID
	}
	if err := s.requireExpectedRuntimeID(
		request.ExpectedRuntimeID,
		"preflight_runtime",
		resource,
	); err != nil {
		writeError(w, err)
		return
	}
	profile := s.runtimeEnforcementProfile()
	if request.RequiredEnforcementProfile != "" &&
		request.RequiredEnforcementProfile != profile {
		writeError(w, &model.KernelError{
			Code:      model.ErrorCapabilityDenied,
			Operation: "preflight_runtime",
			Resource:  resource,
			Message:   "daemon enforcement profile does not satisfy the request",
		})
		return
	}
	if request.Agent != nil &&
		!s.executionPolicy.agentImageAllowed(
			request.Agent.Profile.ImageDigest,
		) {
		writeError(w, &model.KernelError{
			Code:      model.ErrorCapabilityDenied,
			Operation: "preflight_runtime",
			Resource:  request.Agent.Profile.ImageDigest,
			Message:   "OCI image digest is not allowed by daemon policy",
		})
		return
	}
	if s.store == nil ||
		strings.TrimSpace(s.executionPolicy.EnginePath) == "" {
		writeError(w, runtimePreflightUnavailable(
			resource,
			"daemon Runtime engine is unavailable",
			nil,
		))
		return
	}

	ctx, cancel := context.WithTimeout(
		r.Context(),
		runtimePreflightStageTimeout,
	)
	if err := s.store.Health(ctx); err != nil {
		cancel()
		writeError(w, runtimePreflightUnavailable(
			resource,
			"daemon ledger is unavailable",
			err,
		))
		return
	}
	cancel()
	ctx, cancel = context.WithTimeout(
		r.Context(),
		runtimePreflightStageTimeout,
	)
	if err := runReadinessCommand(
		ctx,
		s.executionPolicy.EnginePath,
		"info",
		"--format",
		"{{.ServerVersion}}",
	); err != nil {
		cancel()
		writeError(w, runtimePreflightUnavailable(
			resource,
			"daemon Runtime engine is unavailable",
			err,
		))
		return
	}
	cancel()
	if request.Agent != nil {
		ctx, cancel = context.WithTimeout(
			r.Context(),
			runtimePreflightStageTimeout,
		)
		if err := runReadinessCommand(
			ctx,
			s.executionPolicy.EnginePath,
			"image",
			"inspect",
			"--format",
			"{{.Id}}",
			request.Agent.OCIImage,
		); err != nil {
			cancel()
			writeError(w, runtimePreflightUnavailable(
				request.Agent.Profile.ImageDigest,
				"selected OCI image is unavailable to the daemon",
				err,
			))
			return
		}
		cancel()
	}

	result := runtimepreflight.Result{
		Version:            runtimepreflight.ResultVersion,
		RuntimeID:          s.runtimeID,
		EnforcementProfile: profile,
		Ready:              true,
	}
	if request.Agent != nil {
		result.AgentProfileID = request.Agent.Profile.ID
		result.ImageDigest = request.Agent.Profile.ImageDigest
	}
	if err := result.Validate(); err != nil {
		writeError(w, &model.KernelError{
			Code:      model.ErrorInternal,
			Operation: "preflight_runtime",
			Resource:  resource,
			Message:   "daemon produced an invalid Runtime preflight result",
			Cause:     err,
		})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func runtimePreflightUnavailable(
	resource string,
	message string,
	cause error,
) *model.KernelError {
	return &model.KernelError{
		Code:      model.ErrorDriverUnavailable,
		Operation: "preflight_runtime",
		Resource:  resource,
		Message:   message,
		Cause:     cause,
	}
}
