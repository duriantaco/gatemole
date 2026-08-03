package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"strconv"
	"sync"
	"time"

	"github.com/duriantaco/gatemole/internal/kernel/approval"
	"github.com/duriantaco/gatemole/internal/kernel/broker"
	"github.com/duriantaco/gatemole/internal/kernel/capability"
	"github.com/duriantaco/gatemole/internal/kernel/eventlog"
	"github.com/duriantaco/gatemole/internal/kernel/identity"
	"github.com/duriantaco/gatemole/internal/kernel/model"
	kernelmodelbroker "github.com/duriantaco/gatemole/internal/kernel/modelbroker"
	"github.com/duriantaco/gatemole/internal/kernel/reducer"
	"github.com/duriantaco/gatemole/internal/kernel/runtimeidentity"
	"github.com/duriantaco/gatemole/internal/kernel/store"
	transactionreducer "github.com/duriantaco/gatemole/internal/kernel/transaction"
	"github.com/duriantaco/gatemole/internal/kernel/transaction/gitstage"
	"github.com/duriantaco/gatemole/internal/kernel/verification"
)

const maxRequestBytes = 2 << 20

type Server struct {
	store                 store.Store
	broker                *broker.Broker
	now                   func() time.Time
	mu                    sync.Mutex
	locks                 map[string]*lockEntry
	readinessMu           sync.Mutex
	readinessCheckedAt    time.Time
	readinessErr          error
	workloadSlots         chan struct{}
	gitStage              *gitstage.Manager
	transactionRepository string
	transactionStaging    string
	runtimeID             string
	sequencePolicy        transactionreducer.SequencePolicy
	executionPolicy       ExecutionRuntimePolicy
	approvalTrust         *approval.TrustStore
	releasePolicy         ReleasePolicy
	identityVerifier      *identity.Verifier
}

type Option func(*Server)

type lockEntry struct {
	mutex      sync.Mutex
	references int
}

type resourceLock struct {
	server *Server
	key    string
	entry  *lockEntry
}

type ExecutionRuntimePolicy struct {
	AllowHost                 bool
	AllowExternalExecution    bool
	AllowExternalVerification bool
	AllowedAgentImageDigests  map[string]struct{}
	AllowedAgentImages        []string
	// AllowedImageDigests is retained for development/test compatibility.
	// Production daemon configuration populates AllowedAgentImageDigests and
	// uses VerifierProfiles for the independent verifier trust domain.
	AllowedImageDigests          map[string]struct{}
	VerifierProfiles             *verification.ProfileSet
	VerifierProfilesSourceDigest string
	RequireVerifierProfiles      bool
	EnforcementProfile           string
	IdentityTrustDigest          string
	ApprovalTrustDigest          string
	EnginePath                   string
	VerifierUID                  int
	VerifierGID                  int
	VerifierMemoryBytes          int64
	VerifierCPUMillis            int64
	VerifierPIDsLimit            int64
	VerifierTmpfsBytes           int64
	MaxVerificationTimeoutSecs   int64
	MaxAgentTimeoutSecs          int64
	MaxConcurrentWorkloads       int
	ModelBroker                  *ModelBrokerRuntimePolicy
}

func (policy ExecutionRuntimePolicy) agentImageAllowed(digest string) bool {
	allowed := policy.AllowedAgentImageDigests
	if allowed == nil {
		allowed = policy.AllowedImageDigests
	}
	if len(allowed) == 0 {
		return policy.EnforcementProfile !=
			store.EnforcementProfileProduction
	}
	_, ok := allowed[digest]
	return ok
}

type ModelBrokerRuntimePolicy struct {
	Image               string
	PolicyData          []byte
	PolicyDigest        string
	Policy              kernelmodelbroker.Policy
	ProviderBearerToken string
}

type ReleasePolicy struct {
	AllowedGitRefs []string
}

func (policy ReleasePolicy) AllowsGitRef(target string) bool {
	if len(policy.AllowedGitRefs) == 0 {
		return true
	}
	for _, pattern := range policy.AllowedGitRefs {
		matched, err := path.Match(pattern, target)
		if err == nil && matched {
			return true
		}
	}
	return false
}

func WithBroker(actionBroker *broker.Broker) Option {
	return func(server *Server) { server.broker = actionBroker }
}

func WithClock(clock func() time.Time) Option {
	return func(server *Server) { server.now = clock }
}

func WithTransactionRuntime(manager *gitstage.Manager, repositoryRoot, stagingRoot string, policy transactionreducer.SequencePolicy) Option {
	return func(server *Server) {
		server.gitStage = manager
		server.transactionRepository = repositoryRoot
		server.transactionStaging = stagingRoot
		server.sequencePolicy = policy
	}
}

// WithRuntimeIdentity binds API admissions and preflight responses to the
// stable local Runtime instance that owns the daemon ledger.
func WithRuntimeIdentity(runtimeID string) Option {
	return func(server *Server) {
		server.runtimeID = runtimeID
	}
}

func WithExecutionRuntimePolicy(policy ExecutionRuntimePolicy) Option {
	return func(server *Server) {
		if policy.ModelBroker != nil {
			broker := *policy.ModelBroker
			broker.PolicyData = append([]byte(nil), broker.PolicyData...)
			broker.Policy.AllowedModels = append(
				[]string(nil),
				broker.Policy.AllowedModels...,
			)
			broker.Policy.AllowedToolTypes = append(
				[]string(nil),
				broker.Policy.AllowedToolTypes...,
			)
			if broker.Policy.ProviderHeaders != nil {
				headers := make(
					map[string]string,
					len(broker.Policy.ProviderHeaders),
				)
				for name, value := range broker.Policy.ProviderHeaders {
					headers[name] = value
				}
				broker.Policy.ProviderHeaders = headers
			}
			policy.ModelBroker = &broker
		}
		server.executionPolicy = policy
	}
}

func WithApprovalTrustStore(trust *approval.TrustStore) Option {
	return func(server *Server) {
		server.approvalTrust = trust
	}
}

func WithReleasePolicy(policy ReleasePolicy) Option {
	return func(server *Server) {
		server.releasePolicy = policy
	}
}

func WithIdentityVerifier(verifier *identity.Verifier) Option {
	return func(server *Server) {
		server.identityVerifier = verifier
	}
}

func NewServer(kernelStore store.Store, options ...Option) *Server {
	server := &Server{
		store: kernelStore,
		now:   time.Now,
		locks: map[string]*lockEntry{},
		executionPolicy: ExecutionRuntimePolicy{
			AllowHost:                 true,
			AllowExternalExecution:    true,
			AllowExternalVerification: true,
		},
		approvalTrust: approval.EmptyTrustStore(),
	}
	for _, option := range options {
		option(server)
	}
	if server.executionPolicy.MaxConcurrentWorkloads > 0 {
		server.workloadSlots = make(
			chan struct{},
			server.executionPolicy.MaxConcurrentWorkloads,
		)
	}
	return server
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc(
		"POST /v0/namespaces/{namespace}/runtime/preflight",
		s.preflightRuntime,
	)
	mux.HandleFunc(
		"POST /v0/namespaces/{namespace}/task-admissions",
		s.admitTask,
	)
	mux.HandleFunc(
		"POST /v0/runs",
		s.requireLegacyRunCreation(s.requireLegacyHostRuntime(s.createRun)),
	)
	mux.HandleFunc("GET /v0/namespaces/{namespace}/runs", s.listRuns)
	mux.HandleFunc("GET /v0/namespaces/{namespace}/runs/{runID}", s.getRun)
	mux.HandleFunc("GET /v0/namespaces/{namespace}/runs/{runID}/events", s.listEvents)
	mux.HandleFunc(
		"POST /v0/namespaces/{namespace}/runs/{runID}/events",
		s.requireLegacyHostRuntime(s.appendEvent),
	)
	mux.HandleFunc(
		"POST /v0/namespaces/{namespace}/runs/{runID}/capabilities",
		s.requireLegacyCapabilityCompilation(
			s.requireLegacyHostRuntime(s.compileCapabilities),
		),
	)
	mux.HandleFunc(
		"POST /v0/namespaces/{namespace}/runs/{runID}/actions",
		s.requireLegacyHostRuntime(s.executeAction),
	)
	mux.HandleFunc(
		"POST /v0/transactions",
		s.requireLegacyTransactionCreation(s.createTransaction),
	)
	mux.HandleFunc("GET /v0/namespaces/{namespace}/transactions", s.listTransactions)
	mux.HandleFunc("GET /v0/namespaces/{namespace}/transactions/{transactionID}", s.getTransaction)
	mux.HandleFunc("GET /v0/namespaces/{namespace}/transactions/{transactionID}/events", s.listTransactionEvents)
	mux.HandleFunc("POST /v0/namespaces/{namespace}/transactions/{transactionID}/start", s.startTransaction)
	mux.HandleFunc("POST /v0/namespaces/{namespace}/transactions/{transactionID}/git-worktree", s.createTransactionWorktree)
	mux.HandleFunc("POST /v0/namespaces/{namespace}/transactions/{transactionID}/executions/start", s.startAgentExecution)
	mux.HandleFunc("POST /v0/namespaces/{namespace}/transactions/{transactionID}/executions/finish", s.finishAgentExecution)
	mux.HandleFunc("POST /v0/namespaces/{namespace}/transactions/{transactionID}/executions/run", s.runAgentExecution)
	mux.HandleFunc("POST /v0/namespaces/{namespace}/transactions/{transactionID}/stage", s.stageTransaction)
	mux.HandleFunc("POST /v0/namespaces/{namespace}/transactions/{transactionID}/validate", s.validateTransaction)
	mux.HandleFunc("POST /v0/namespaces/{namespace}/transactions/{transactionID}/verifications", s.recordTransactionVerification)
	mux.HandleFunc("POST /v0/namespaces/{namespace}/transactions/{transactionID}/verifications/run", s.runTransactionVerification)
	mux.HandleFunc("POST /v0/namespaces/{namespace}/transactions/{transactionID}/prepare", s.prepareTransaction)
	mux.HandleFunc("POST /v0/namespaces/{namespace}/transactions/{transactionID}/renew", s.renewTransactionAuthority)
	mux.HandleFunc("POST /v0/namespaces/{namespace}/transactions/{transactionID}/approvals", s.resolveTransactionApproval)
	mux.HandleFunc("POST /v0/namespaces/{namespace}/transactions/{transactionID}/release", s.releaseTransaction)
	mux.HandleFunc("POST /v0/namespaces/{namespace}/transactions/{transactionID}/abort", s.abortTransaction)
	var handler http.Handler = s.requireRuntimeHeader(mux)
	if s.identityVerifier != nil {
		return s.identityMiddleware(handler)
	}
	return handler
}

func (s *Server) requireLegacyHostRuntime(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.executionPolicy.AllowHost {
			writeError(w, &model.KernelError{
				Code:      model.ErrorCapabilityDenied,
				Operation: "legacy_host_runtime",
				Resource:  r.URL.Path,
				Message:   "legacy host run and filesystem action APIs are disabled by daemon policy",
			})
			return
		}
		next(w, r)
	}
}

func (s *Server) requireLegacyRunCreation(
	next http.HandlerFunc,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if runtimeidentity.IsRuntimeID(s.runtimeID) {
			writeError(w, &model.KernelError{
				Code:      model.ErrorCapabilityDenied,
				Operation: "legacy_run_creation",
				Resource:  r.URL.Path,
				Message:   "configured Runtime runs require atomic task admission",
			})
			return
		}
		next(w, r)
	}
}

func (s *Server) requireLegacyCapabilityCompilation(
	next http.HandlerFunc,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if runtimeidentity.IsRuntimeID(s.runtimeID) {
			writeError(w, &model.KernelError{
				Code:      model.ErrorCapabilityDenied,
				Operation: "legacy_capability_compilation",
				Resource:  r.URL.Path,
				Message:   "configured Runtime capabilities are installed atomically by task admission",
			})
			return
		}
		next(w, r)
	}
}

func (s *Server) requireLegacyTransactionCreation(
	next http.HandlerFunc,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if runtimeidentity.IsRuntimeID(s.runtimeID) ||
			s.executionPolicy.EnforcementProfile ==
				store.EnforcementProfileProduction {
			message := "production transactions require atomic task admission"
			if runtimeidentity.IsRuntimeID(s.runtimeID) {
				message = "configured Runtime transactions require atomic task admission"
			}
			writeError(w, &model.KernelError{
				Code:      model.ErrorCapabilityDenied,
				Operation: "legacy_transaction_creation",
				Resource:  r.URL.Path,
				Message:   message,
			})
			return
		}
		next(w, r)
	}
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
	defer cancel()
	if s.executionPolicy.RequireVerifierProfiles &&
		s.executionPolicy.VerifierProfiles == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
		return
	}
	if err := s.dependenciesReady(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	var event model.RunEvent
	if err := decodeBody(w, r, &event); err != nil {
		writeError(w, err)
		return
	}
	lock := s.runLock("", event.RunID)
	lock.Lock()
	defer lock.Unlock()
	projection, err := s.store.CreateRun(r.Context(), event)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, projection)
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.store.ListRuns(r.Context(), r.PathValue("namespace"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	projection, err := s.store.GetRun(r.Context(), r.PathValue("namespace"), r.PathValue("runID"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, projection)
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	after := int64(0)
	if raw := r.URL.Query().Get("after"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 0 {
			writeError(w, schemaError("after must be a non-negative integer", err))
			return
		}
		after = value
	}
	events, err := s.store.Events(
		r.Context(),
		r.PathValue("namespace"),
		r.PathValue("runID"),
		after,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

type appendEventRequest struct {
	ExpectedSequence int64          `json:"expected_sequence"`
	Event            model.RunEvent `json:"event"`
}

func (s *Server) appendEvent(w http.ResponseWriter, r *http.Request) {
	var request appendEventRequest
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	if request.Event.RunID != r.PathValue("runID") {
		writeError(w, schemaError("event run_id does not match request path", nil))
		return
	}
	if request.Event.Type != reducer.EventRunStateChanged {
		writeError(w, &model.KernelError{
			Code: model.ErrorCapabilityDenied, Operation: "append_event", Resource: request.Event.ID,
			Message: "privileged kernel events must be produced by their mediated endpoint",
		})
		return
	}
	lock := s.runLock(r.PathValue("namespace"), r.PathValue("runID"))
	lock.Lock()
	defer lock.Unlock()
	if err := s.requireCurrentRuntimeRunAuthority(
		r.Context(),
		r.PathValue("namespace"),
		request.Event.RunID,
	); err != nil {
		writeError(w, err)
		return
	}
	projection, err := s.store.AppendEvent(
		r.Context(),
		r.PathValue("namespace"),
		request.ExpectedSequence,
		request.Event,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, projection)
}

type compileCapabilitiesRequest struct {
	ExpectedSequence int64                   `json:"expected_sequence"`
	Contract         model.ExecutionContract `json:"contract"`
	Actor            model.Principal         `json:"actor"`
}

type compileCapabilitiesResponse struct {
	Projection reducer.Projection      `json:"projection"`
	Grants     []model.CapabilityGrant `json:"grants"`
}

func (s *Server) compileCapabilities(w http.ResponseWriter, r *http.Request) {
	var request compileCapabilitiesRequest
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	namespace := r.PathValue("namespace")
	runID := r.PathValue("runID")
	lock := s.runLock(namespace, runID)
	lock.Lock()
	defer lock.Unlock()
	if err := s.requireCurrentRuntimeRunAuthority(
		r.Context(),
		namespace,
		runID,
	); err != nil {
		writeError(w, err)
		return
	}
	projection, err := s.store.GetRun(r.Context(), namespace, runID)
	if err != nil {
		writeError(w, err)
		return
	}
	if projection.Run.EventSequence != request.ExpectedSequence {
		writeError(w, &model.KernelError{
			Code: model.ErrorConflict, Operation: "compile_capabilities", Resource: runID,
			Message: "run changed before capabilities could be installed",
		})
		return
	}
	grants, err := capability.Compile(request.Contract, projection.Run, s.now().UTC())
	if err != nil {
		writeError(w, err)
		return
	}
	event, err := eventlog.Next(projection, reducer.EventCapabilitiesGranted, request.Actor, s.now().UTC(),
		model.CapabilitiesGrantedPayload{ContractDigest: request.Contract.Digest, Grants: grants})
	if err != nil {
		writeError(w, err)
		return
	}
	if len(grants) > 0 {
		event.PolicyDecisionID = grants[0].PolicyDecisionID
		event.Digest, err = model.ComputeEventDigest(event)
		if err != nil {
			writeError(w, err)
			return
		}
	}
	projection, err = s.store.AppendEvent(r.Context(), namespace, request.ExpectedSequence, event)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, compileCapabilitiesResponse{Projection: projection, Grants: grants})
}

func (s *Server) executeAction(w http.ResponseWriter, r *http.Request) {
	if s.broker == nil {
		writeError(w, &model.KernelError{
			Code: model.ErrorDriverUnavailable, Operation: "execute_action", Message: "action broker is not configured",
		})
		return
	}
	var request broker.ExecuteRequest
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	if request.Action.RunID != r.PathValue("runID") {
		writeError(w, schemaError("action run_id does not match request path", nil))
		return
	}
	namespace := r.PathValue("namespace")
	lock := s.runLock(namespace, r.PathValue("runID"))
	lock.Lock()
	defer lock.Unlock()
	if err := s.requireCurrentRuntimeRunAuthority(
		r.Context(),
		namespace,
		request.Action.RunID,
	); err != nil {
		writeError(w, err)
		return
	}
	outcome, err := s.broker.Execute(r.Context(), namespace, request)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, outcome)
}

func (s *Server) runLock(namespace, runID string) *resourceLock {
	key := namespace + "\x00" + runID
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.locks[key]
	if entry == nil {
		entry = &lockEntry{}
		s.locks[key] = entry
	}
	entry.references++
	return &resourceLock{server: s, key: key, entry: entry}
}

func (lock *resourceLock) Lock() {
	lock.entry.mutex.Lock()
}

func (lock *resourceLock) Unlock() {
	lock.entry.mutex.Unlock()
	lock.server.mu.Lock()
	defer lock.server.mu.Unlock()
	lock.entry.references--
	if lock.entry.references == 0 &&
		lock.server.locks[lock.key] == lock.entry {
		delete(lock.server.locks, lock.key)
	}
}

func decodeBody(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return schemaError("invalid JSON request", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return schemaError("trailing JSON is not allowed", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	var kernelErr *model.KernelError
	if errors.As(err, &kernelErr) {
		switch kernelErr.Code {
		case model.ErrorSchemaInvalid, model.ErrorUnknownField, model.ErrorIdentityInvalid,
			model.ErrorTransitionInvalid, model.ErrorEventSequence, model.ErrorEventChain:
			status = http.StatusBadRequest
		case model.ErrorNotFound:
			status = http.StatusNotFound
		case model.ErrorConflict, model.ErrorIdempotencyConflict,
			model.ErrorTransactionConflict,
			model.ErrorCheckpointIncompatible:
			status = http.StatusConflict
		case model.ErrorCapabilityDenied, model.ErrorCapabilityExpired, model.ErrorCapabilityRevoked,
			model.ErrorDelegationExceeded, model.ErrorBudgetExceeded:
			status = http.StatusForbidden
		case model.ErrorApprovalRequired:
			status = http.StatusAccepted
		case model.ErrorVerificationFailed, model.ErrorPartialCommit:
			status = http.StatusUnprocessableEntity
		case model.ErrorDriverUnavailable:
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, kernelErr)
		return
	}
	writeJSON(w, status, &model.KernelError{
		Code:    model.ErrorInternal,
		Message: "unexpected kernel API failure",
	})
}

func schemaError(message string, cause error) *model.KernelError {
	return &model.KernelError{
		Code:      model.ErrorSchemaInvalid,
		Operation: "http_request",
		Message:   message,
		Cause:     cause,
	}
}
