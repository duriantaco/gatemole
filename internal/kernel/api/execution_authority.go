package api

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/authority"
	"github.com/duriantaco/vouch/internal/kernel/model"
	"github.com/duriantaco/vouch/internal/kernel/sandbox"
	"github.com/duriantaco/vouch/internal/kernel/store"
	transactionreducer "github.com/duriantaco/vouch/internal/kernel/transaction"
)

type liveExecutionAuthority struct {
	Plan               authority.ExecutionPlan
	RunSequence        int64
	RunLastEventDigest string
}

func (s *Server) compileLiveExecutionAuthority(
	ctx context.Context,
	namespace, transactionID string,
	current transactionreducer.Projection,
	image string,
	command []string,
	timeoutSeconds int64,
) (liveExecutionAuthority, error) {
	snapshot, err := s.store.GetExecutionAuthority(
		ctx,
		namespace,
		transactionID,
	)
	if err != nil {
		var kernelErr *model.KernelError
		if errors.As(err, &kernelErr) &&
			kernelErr.Code == model.ErrorNotFound {
			return liveExecutionAuthority{}, &model.KernelError{
				Code:      model.ErrorCapabilityDenied,
				Operation: "compile_execution_authority",
				Resource:  transactionID,
				Message:   "daemon-owned execution requires atomic task admission",
				Cause:     err,
			}
		}
		return liveExecutionAuthority{}, err
	}
	if snapshot.Transaction.Transaction.EventSequence !=
		current.Transaction.EventSequence ||
		snapshot.Transaction.LastEventDigest != current.LastEventDigest {
		return liveExecutionAuthority{}, &model.KernelError{
			Code:      model.ErrorConflict,
			Operation: "compile_execution_authority",
			Resource:  transactionID,
			Message:   "transaction changed while execution authority was loaded",
		}
	}
	ceilings, err := s.executionAuthorityCeilings(image)
	if err != nil {
		return liveExecutionAuthority{}, err
	}
	var callerTimeout *int64
	if timeoutSeconds > 0 {
		callerTimeout = &timeoutSeconds
	}
	plan, err := authority.Compile(authority.CompileInput{
		Task:                 snapshot.Task,
		Contract:             snapshot.Contract,
		Run:                  snapshot.Run.Run,
		Grants:               snapshot.Grants,
		Transaction:          snapshot.Transaction.Transaction,
		Executions:           append([]model.AgentExecution(nil), snapshot.Transaction.Executions...),
		ImageReference:       image,
		Command:              append([]string(nil), command...),
		Ceilings:             ceilings,
		CallerTimeoutSeconds: callerTimeout,
		Now:                  s.now().UTC(),
	})
	if err != nil {
		return liveExecutionAuthority{}, err
	}
	return liveExecutionAuthority{
		Plan:               plan,
		RunSequence:        snapshot.Run.Run.EventSequence,
		RunLastEventDigest: snapshot.Run.LastEventDigest,
	}, nil
}

func (s *Server) requireLiveExecutionAuthority(
	ctx context.Context,
	transactionID string,
	notAfter time.Time,
) error {
	if !s.now().UTC().Before(notAfter) {
		return &model.KernelError{
			Code:      model.ErrorCapabilityExpired,
			Operation: "claim_execution_authority",
			Resource:  transactionID,
			Message:   "execution authority expired before workload launch",
		}
	}
	if err := ctx.Err(); err != nil {
		return &model.KernelError{
			Code:      model.ErrorCapabilityExpired,
			Operation: "claim_execution_authority",
			Resource:  transactionID,
			Message:   "execution authority ended before workload launch",
			Cause:     err,
		}
	}
	return nil
}

func (s *Server) executionAuthorityCeilings(
	requestedImage string,
) (authority.DaemonCeilings, error) {
	allowed := s.executionPolicy.AllowedAgentImageDigests
	if allowed == nil {
		allowed = s.executionPolicy.AllowedImageDigests
	}
	digests := make([]string, 0, len(allowed))
	for digest := range allowed {
		digests = append(digests, digest)
	}
	if len(digests) == 0 {
		if s.executionPolicy.EnforcementProfile ==
			store.EnforcementProfileProduction {
			return authority.DaemonCeilings{}, &model.KernelError{
				Code:      model.ErrorCapabilityDenied,
				Operation: "compile_execution_authority",
				Resource:  requestedImage,
				Message:   "production execution requires an agent image allowlist",
			}
		}
		digest, err := sandbox.ImageDigest(requestedImage)
		if err != nil {
			return authority.DaemonCeilings{}, &model.KernelError{
				Code:      model.ErrorSchemaInvalid,
				Operation: "compile_execution_authority",
				Resource:  requestedImage,
				Message:   "agent image must be digest pinned",
				Cause:     err,
			}
		}
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	ceilings := authority.DaemonCeilings{
		MaxWallTimeSeconds:  s.executionPolicy.MaxAgentTimeoutSecs,
		AllowedImageDigests: digests,
	}
	if broker := s.executionPolicy.ModelBroker; broker != nil {
		imageDigest, err := sandbox.ImageDigest(broker.Image)
		if err != nil {
			return authority.DaemonCeilings{}, &model.KernelError{
				Code:      model.ErrorSchemaInvalid,
				Operation: "compile_execution_authority",
				Resource:  broker.Image,
				Message:   "model broker image must be digest pinned",
				Cause:     err,
			}
		}
		ceilings.ModelBroker = &authority.ModelBrokerCeiling{
			Provider:                  broker.Policy.Provider,
			AllowedModels:             append([]string(nil), broker.Policy.AllowedModels...),
			ImageDigest:               imageDigest,
			PolicyDigest:              broker.PolicyDigest,
			MaxModelCalls:             broker.Policy.MaxRequests,
			MaxInputTokens:            broker.Policy.MaxTotalInputTokens,
			MaxOutputTokens:           broker.Policy.MaxTotalOutputTokens,
			MaxOutputTokensPerRequest: broker.Policy.MaxOutputTokensPerRequest,
		}
	}
	return ceilings, nil
}
