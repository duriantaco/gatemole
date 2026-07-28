package model

import "fmt"

type ErrorCode string

const (
	ErrorSchemaInvalid          ErrorCode = "KERNEL_SCHEMA_INVALID"
	ErrorUnknownField           ErrorCode = "KERNEL_UNKNOWN_FIELD"
	ErrorIdentityInvalid        ErrorCode = "KERNEL_IDENTITY_INVALID"
	ErrorTransitionInvalid      ErrorCode = "KERNEL_TRANSITION_INVALID"
	ErrorEventSequence          ErrorCode = "KERNEL_EVENT_SEQUENCE"
	ErrorEventChain             ErrorCode = "KERNEL_EVENT_CHAIN"
	ErrorCapabilityDenied       ErrorCode = "KERNEL_CAPABILITY_DENIED"
	ErrorCapabilityExpired      ErrorCode = "KERNEL_CAPABILITY_EXPIRED"
	ErrorCapabilityRevoked      ErrorCode = "KERNEL_CAPABILITY_REVOKED"
	ErrorDelegationExceeded     ErrorCode = "KERNEL_DELEGATION_EXCEEDED"
	ErrorApprovalRequired       ErrorCode = "KERNEL_APPROVAL_REQUIRED"
	ErrorApprovalInvalid        ErrorCode = "KERNEL_APPROVAL_INVALID"
	ErrorBudgetExceeded         ErrorCode = "KERNEL_BUDGET_EXCEEDED"
	ErrorActionUnknown          ErrorCode = "KERNEL_ACTION_UNKNOWN"
	ErrorCheckpointIncompatible ErrorCode = "KERNEL_CHECKPOINT_INCOMPATIBLE"
	ErrorNotFound               ErrorCode = "KERNEL_NOT_FOUND"
	ErrorConflict               ErrorCode = "KERNEL_CONFLICT"
	ErrorIdempotencyConflict    ErrorCode = "KERNEL_IDEMPOTENCY_CONFLICT"
	ErrorDriverUnavailable      ErrorCode = "KERNEL_DRIVER_UNAVAILABLE"
	ErrorTransactionConflict    ErrorCode = "TRANSACTION_CONFLICT"
	ErrorEffectSequence         ErrorCode = "TRANSACTION_EFFECT_SEQUENCE"
	ErrorVerificationFailed     ErrorCode = "TRANSACTION_VERIFICATION_FAILED"
	ErrorPartialCommit          ErrorCode = "TRANSACTION_PARTIAL_COMMIT"
	ErrorInternal               ErrorCode = "KERNEL_INTERNAL"
)

// KernelError is the stable machine-readable error boundary for kernel APIs.
// Message text may improve without changing Code.
type KernelError struct {
	Code      ErrorCode `json:"code"`
	Operation string    `json:"operation,omitempty"`
	Resource  string    `json:"resource,omitempty"`
	Field     string    `json:"field,omitempty"`
	Message   string    `json:"message"`
	Cause     error     `json:"-"`
}

func (e *KernelError) Error() string {
	prefix := string(e.Code)
	if e.Operation != "" {
		prefix += " " + e.Operation
	}
	if e.Field != "" {
		return fmt.Sprintf("%s: %s: %s", prefix, e.Field, e.Message)
	}
	return fmt.Sprintf("%s: %s", prefix, e.Message)
}

func (e *KernelError) Unwrap() error {
	return e.Cause
}

func newError(code ErrorCode, operation, resource, field, message string, cause error) *KernelError {
	return &KernelError{
		Code:      code,
		Operation: operation,
		Resource:  resource,
		Field:     field,
		Message:   message,
		Cause:     cause,
	}
}
