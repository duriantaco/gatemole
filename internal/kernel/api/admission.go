package api

import (
	"errors"
	"net/http"

	"github.com/duriantaco/vouch/internal/kernel/admission"
	"github.com/duriantaco/vouch/internal/kernel/model"
)

func (s *Server) admitTask(w http.ResponseWriter, r *http.Request) {
	var request admission.Request
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	namespace := r.PathValue("namespace")
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
