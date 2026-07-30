package api

import (
	"errors"
	"net/http"

	"github.com/duriantaco/gatemole/internal/kernel/admission"
	"github.com/duriantaco/gatemole/internal/kernel/model"
	"github.com/duriantaco/gatemole/internal/kernel/runtimeidentity"
)

func (s *Server) admitTask(w http.ResponseWriter, r *http.Request) {
	var request admission.Request
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	namespace := r.PathValue("namespace")
	switch request.Version {
	case admission.LegacyRequestVersion:
		if runtimeidentity.IsRuntimeID(s.runtimeID) {
			s.replayLegacyTaskAdmission(w, r, namespace, request)
			return
		}
	case admission.RequestVersion:
		// Runtime-bound admission continues below.
	default:
		writeError(w, schemaError(
			"invalid task admission request",
			errors.New("unsupported task admission version"),
		))
		return
	}
	if err := s.requireExpectedRuntimeID(
		request.ExpectedRuntimeID,
		"admit_task",
		request.TransactionID,
	); err != nil {
		writeError(w, err)
		return
	}
	if request.Version == admission.RequestVersion &&
		request.ExpectedEnforcementProfile !=
			s.runtimeEnforcementProfile() {
		writeError(w, &model.KernelError{
			Code:      model.ErrorCapabilityDenied,
			Operation: "admit_task",
			Resource:  request.TransactionID,
			Message:   "request enforcement profile does not match this daemon",
		})
		return
	}
	requestDigest, err := admission.ComputeRequestDigest(namespace, request)
	if err != nil {
		writeError(w, err)
		return
	}

	existing, storedDigest, err := s.store.GetTaskAdmission(
		r.Context(),
		namespace,
		request.IdempotencyKey,
	)
	switch {
	case err == nil:
		if storedDigest != requestDigest {
			writeError(w, idempotencyConflict(request.IdempotencyKey))
			return
		}
		writeJSON(w, http.StatusOK, existing)
		return
	case !isNotFound(err):
		writeError(w, err)
		return
	}

	switch request.AgentProfile.RuntimeClass {
	case "host":
		if !s.executionPolicy.AllowHost {
			writeError(w, &model.KernelError{
				Code:      model.ErrorCapabilityDenied,
				Operation: "admit_task",
				Resource:  request.TransactionID,
				Message:   "host execution is disabled by daemon policy",
			})
			return
		}
	case "oci":
		if !s.executionPolicy.agentImageAllowed(
			request.AgentProfile.ImageDigest,
		) {
			writeError(w, &model.KernelError{
				Code:      model.ErrorCapabilityDenied,
				Operation: "admit_task",
				Resource:  request.AgentProfile.ImageDigest,
				Message:   "OCI image digest is not allowed by daemon policy",
			})
			return
		}
	}

	prepared, err := admission.Prepare(namespace, request, s.now().UTC())
	if err != nil {
		writeError(w, err)
		return
	}
	result, created, err := s.store.AdmitTask(r.Context(), prepared)
	if err != nil {
		writeError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, result)
}

func (s *Server) replayLegacyTaskAdmission(
	w http.ResponseWriter,
	r *http.Request,
	namespace string,
	request admission.Request,
) {
	if err := s.requireExpectedRuntimeID(
		r.Header.Get(runtimeidentity.HTTPHeader),
		"admit_task",
		request.IdempotencyKey,
	); err != nil {
		writeError(w, err)
		return
	}
	if request.ExpectedRuntimeID != "" ||
		request.ExpectedEnforcementProfile != "" {
		writeError(w, schemaError(
			"invalid legacy task admission request",
			errors.New("v0 admission cannot carry Runtime metadata"),
		))
		return
	}
	requestDigest, err := admission.ComputeRequestDigest(namespace, request)
	if err != nil {
		writeError(w, err)
		return
	}
	existing, storedDigest, err := s.store.GetTaskAdmission(
		r.Context(),
		namespace,
		request.IdempotencyKey,
	)
	switch {
	case isNotFound(err):
		writeError(w, &model.KernelError{
			Code:      model.ErrorCheckpointIncompatible,
			Operation: "admit_task",
			Resource:  request.IdempotencyKey,
			Message:   "configured Runtime may replay but cannot create legacy v0 admission",
		})
	case err != nil:
		writeError(w, err)
	case existing.Version != admission.LegacyResultVersion:
		writeError(w, idempotencyConflict(request.IdempotencyKey))
	case storedDigest != requestDigest:
		writeError(w, idempotencyConflict(request.IdempotencyKey))
	default:
		writeJSON(w, http.StatusOK, existing)
	}
}

func isNotFound(err error) bool {
	var kernelErr *model.KernelError
	return errors.As(err, &kernelErr) &&
		kernelErr.Code == model.ErrorNotFound
}

func idempotencyConflict(key string) *model.KernelError {
	return &model.KernelError{
		Code:      model.ErrorIdempotencyConflict,
		Operation: "admit_task",
		Resource:  key,
		Message:   "idempotency key is already bound to different admission input",
	}
}
