package api

import (
	"net/http"
	"strings"

	"github.com/duriantaco/vouch/internal/kernel/model"
	"github.com/duriantaco/vouch/internal/kernel/runtimeidentity"
	"github.com/duriantaco/vouch/internal/kernel/store"
)

func (s *Server) requireRuntimeHeader(
	next http.Handler,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		if s.runtimeID != "" &&
			!runtimeidentity.IsRuntimeID(s.runtimeID) {
			writeError(w, &model.KernelError{
				Code:      model.ErrorDriverUnavailable,
				Operation: "bind_runtime_request",
				Resource:  r.URL.Path,
				Message:   "daemon Runtime identity is invalid",
			})
			return
		}
		expected := r.Header.Get(runtimeidentity.HTTPHeader)
		if expected == "" {
			if s.runtimeID == "" ||
				runtimeIdentityInRequestBody(r.URL.Path) {
				// Embedded development servers may be deliberately
				// unbound. Preflight and admission carry their required
				// Runtime identity in the validated request body.
				next.ServeHTTP(w, r)
				return
			}
			writeError(w, &model.KernelError{
				Code:      model.ErrorCapabilityDenied,
				Operation: "bind_runtime_request",
				Resource:  r.URL.Path,
				Message:   "request is missing the daemon Runtime identity",
			})
			return
		}
		if !runtimeidentity.IsRuntimeID(s.runtimeID) ||
			!runtimeidentity.IsRuntimeID(expected) ||
			expected != s.runtimeID {
			writeError(w, &model.KernelError{
				Code:      model.ErrorCapabilityDenied,
				Operation: "bind_runtime_request",
				Resource:  r.URL.Path,
				Message:   "request Runtime identity does not match this daemon",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func runtimeIdentityInRequestBody(requestPath string) bool {
	return strings.HasSuffix(requestPath, "/runtime/preflight") ||
		strings.HasSuffix(requestPath, "/task-admissions")
}

func (s *Server) requireExpectedRuntimeID(
	expected string,
	operation string,
	resource string,
) error {
	if !runtimeidentity.IsRuntimeID(s.runtimeID) {
		if s.runtimeID != "" ||
			expected != "" ||
			s.executionPolicy.EnforcementProfile ==
				store.EnforcementProfileProduction {
			return &model.KernelError{
				Code:      model.ErrorDriverUnavailable,
				Operation: operation,
				Resource:  resource,
				Message:   "daemon Runtime identity is not configured",
			}
		}
		// Embedded development/test servers that predate Runtime instance
		// binding remain usable. daemon.Run always configures an identity, and
		// production never permits this compatibility path.
		return nil
	}
	if expected == "" || !runtimeidentity.IsRuntimeID(expected) ||
		expected != s.runtimeID {
		return &model.KernelError{
			Code:      model.ErrorCapabilityDenied,
			Operation: operation,
			Resource:  resource,
			Message:   "request Runtime identity does not match this daemon",
		}
	}
	return nil
}

func (s *Server) runtimeEnforcementProfile() string {
	profile := s.executionPolicy.EnforcementProfile
	if profile == "" {
		return store.EnforcementProfileDevelopment
	}
	return profile
}
