package modelbroker

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	CallEventVersion = "gatemole.model_call_event.v0"
	CallStarted      = "started"
	CallCompleted    = "completed"
	CallFailed       = "failed"
	CallUnknown      = "unknown"
)

type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
	Estimated    bool  `json:"estimated,omitempty"`
}

type CallEvent struct {
	Version             string    `json:"version"`
	ID                  string    `json:"id"`
	Sequence            int64     `json:"sequence"`
	TransactionID       string    `json:"transaction_id"`
	RunID               string    `json:"run_id"`
	CallID              string    `json:"call_id"`
	Phase               string    `json:"phase"`
	Provider            string    `json:"provider"`
	Model               string    `json:"model"`
	RequestDigest       string    `json:"request_digest"`
	MaxOutputTokens     int64     `json:"max_output_tokens"`
	HTTPStatus          int       `json:"http_status,omitempty"`
	ResponseDigest      string    `json:"response_digest,omitempty"`
	ProviderRequestID   string    `json:"provider_request_id,omitempty"`
	Usage               Usage     `json:"usage"`
	ErrorCode           string    `json:"error_code,omitempty"`
	OccurredAt          time.Time `json:"occurred_at"`
	PreviousEventDigest string    `json:"previous_event_digest,omitempty"`
	Digest              string    `json:"digest"`
}

type Recorder struct {
	mu            sync.Mutex
	file          *os.File
	transactionID string
	runID         string
	provider      string
	sequence      int64
	lastDigest    string
	requests      int64
	inputTokens   int64
	outputTokens  int64
	completed     int64
	failed        int64
	unknown       int64
	pending       map[string]CallEvent
}

func OpenRecorder(path, transactionID, runID, provider string) (*Recorder, error) {
	if !identifier(transactionID) || !identifier(runID) || !identifier(provider) {
		return nil, errors.New("model receipt identity is invalid")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create model receipt directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open model receipt ledger: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("restrict model receipt ledger: %w", err)
	}
	recorder := &Recorder{
		file: file, transactionID: transactionID, runID: runID, provider: provider,
		pending: make(map[string]CallEvent),
	}
	if err := recorder.load(); err != nil {
		_ = file.Close()
		return nil, err
	}
	for _, started := range recorder.pendingCalls() {
		_, err := recorder.Finish(started, CallUnknown, CallEvent{
			ErrorCode:  "broker_restarted",
			OccurredAt: time.Now().UTC(),
		})
		if err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("close interrupted model call %s: %w", started.CallID, err)
		}
	}
	return recorder, nil
}

func (recorder *Recorder) Close() error {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.file == nil {
		return nil
	}
	err := recorder.file.Close()
	recorder.file = nil
	return err
}

func (recorder *Recorder) Start(
	model, requestDigest string,
	maxOutputTokens int64,
	now time.Time,
) (CallEvent, error) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	callSequence := recorder.sequence + 1
	callID := fmt.Sprintf("model-call:%s:%d", shortDigest(recorder.transactionID), callSequence)
	event, err := recorder.appendLocked(CallEvent{
		Version: CallEventVersion, CallID: callID, Phase: CallStarted,
		Provider: recorder.provider, Model: model, RequestDigest: requestDigest,
		MaxOutputTokens: maxOutputTokens, OccurredAt: now.UTC(),
	})
	if err == nil {
		recorder.requests++
		recorder.pending[event.CallID] = event
	}
	return event, err
}

func (recorder *Recorder) Finish(started CallEvent, phase string, completion CallEvent) (CallEvent, error) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if started.Phase != CallStarted || started.CallID == "" ||
		started.TransactionID != recorder.transactionID ||
		started.RunID != recorder.runID {
		return CallEvent{}, errors.New("model call start receipt is invalid")
	}
	if phase != CallCompleted && phase != CallFailed && phase != CallUnknown {
		return CallEvent{}, errors.New("model call terminal phase is invalid")
	}
	if !validUsage(completion.Usage, phase == CallCompleted) {
		return CallEvent{}, errors.New("model call terminal usage is invalid")
	}
	completion.Version = CallEventVersion
	completion.CallID = started.CallID
	completion.Phase = phase
	completion.Provider = started.Provider
	completion.Model = started.Model
	completion.RequestDigest = started.RequestDigest
	completion.MaxOutputTokens = started.MaxOutputTokens
	completion.OccurredAt = completion.OccurredAt.UTC()
	event, err := recorder.appendLocked(completion)
	if err == nil {
		delete(recorder.pending, started.CallID)
		recorder.countTerminal(phase)
		recorder.inputTokens = saturatingAdd(
			recorder.inputTokens, completion.Usage.InputTokens,
		)
		if completion.Usage.OutputTokens > 0 {
			recorder.outputTokens = saturatingAdd(
				recorder.outputTokens, completion.Usage.OutputTokens,
			)
		} else {
			recorder.outputTokens = saturatingAdd(
				recorder.outputTokens, started.MaxOutputTokens,
			)
		}
	}
	return event, err
}

func (recorder *Recorder) appendLocked(event CallEvent) (CallEvent, error) {
	if recorder.file == nil {
		return CallEvent{}, errors.New("model receipt recorder is closed")
	}
	event.Sequence = recorder.sequence + 1
	event.ID = fmt.Sprintf("model-event:%s:%d", shortDigest(recorder.transactionID), event.Sequence)
	event.TransactionID = recorder.transactionID
	event.RunID = recorder.runID
	event.PreviousEventDigest = recorder.lastDigest
	digest, err := computeEventDigest(event)
	if err != nil {
		return CallEvent{}, err
	}
	event.Digest = digest
	data, err := json.Marshal(event)
	if err != nil {
		return CallEvent{}, err
	}
	data = append(data, '\n')
	if _, err := recorder.file.Write(data); err != nil {
		return CallEvent{}, fmt.Errorf("append model receipt: %w", err)
	}
	if err := recorder.file.Sync(); err != nil {
		return CallEvent{}, fmt.Errorf("sync model receipt: %w", err)
	}
	recorder.sequence = event.Sequence
	recorder.lastDigest = event.Digest
	return event, nil
}

type BudgetSnapshot struct {
	Requests     int64
	InputTokens  int64
	OutputTokens int64
	Completed    int64
	Failed       int64
	Unknown      int64
}

type LedgerSummary struct {
	Digest       string
	Calls        int64
	InputTokens  int64
	OutputTokens int64
	Completed    int64
	Failed       int64
	Unknown      int64
}

func FinalizeLedger(path, transactionID, runID, provider string) (LedgerSummary, error) {
	recorder, err := OpenRecorder(path, transactionID, runID, provider)
	if err != nil {
		return LedgerSummary{}, err
	}
	snapshot := recorder.BudgetSnapshot()
	if err := recorder.Close(); err != nil {
		return LedgerSummary{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return LedgerSummary{}, fmt.Errorf("open finalized model receipt ledger: %w", err)
	}
	defer file.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, file); err != nil {
		return LedgerSummary{}, fmt.Errorf("digest finalized model receipt ledger: %w", err)
	}
	return LedgerSummary{
		Digest: "sha256:" + hex.EncodeToString(sum.Sum(nil)),
		Calls:  snapshot.Requests, InputTokens: snapshot.InputTokens,
		OutputTokens: snapshot.OutputTokens, Completed: snapshot.Completed,
		Failed: snapshot.Failed, Unknown: snapshot.Unknown,
	}, nil
}

func (recorder *Recorder) BudgetSnapshot() BudgetSnapshot {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return BudgetSnapshot{
		Requests: recorder.requests, InputTokens: recorder.inputTokens,
		OutputTokens: recorder.outputTokens, Completed: recorder.completed,
		Failed: recorder.failed, Unknown: recorder.unknown,
	}
}

func (recorder *Recorder) load() error {
	if _, err := recorder.file.Seek(0, 0); err != nil {
		return fmt.Errorf("seek model receipt ledger: %w", err)
	}
	scanner := bufio.NewScanner(recorder.file)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	expectedSequence := int64(1)
	lastDigest := ""
	for scanner.Scan() {
		decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		decoder.DisallowUnknownFields()
		var event CallEvent
		if err := decoder.Decode(&event); err != nil {
			return fmt.Errorf("decode model receipt event %d: %w", expectedSequence, err)
		}
		if event.Version != CallEventVersion ||
			event.Sequence != expectedSequence ||
			event.TransactionID != recorder.transactionID ||
			event.RunID != recorder.runID ||
			event.Provider != recorder.provider ||
			event.PreviousEventDigest != lastDigest {
			return fmt.Errorf("model receipt event %d has an invalid chain or identity", expectedSequence)
		}
		digest, err := computeEventDigest(event)
		if err != nil || digest != event.Digest {
			return fmt.Errorf("model receipt event %d digest is invalid", expectedSequence)
		}
		switch event.Phase {
		case CallStarted:
			if _, duplicate := recorder.pending[event.CallID]; duplicate {
				return fmt.Errorf("model call %s started twice", event.CallID)
			}
			recorder.pending[event.CallID] = event
			recorder.requests++
		case CallCompleted, CallFailed, CallUnknown:
			started, exists := recorder.pending[event.CallID]
			if !exists || event.Model != started.Model ||
				event.RequestDigest != started.RequestDigest ||
				event.MaxOutputTokens != started.MaxOutputTokens {
				return fmt.Errorf("model call %s terminal event is unbound", event.CallID)
			}
			if !validUsage(event.Usage, event.Phase == CallCompleted) {
				return fmt.Errorf("model call %s terminal usage is invalid", event.CallID)
			}
			delete(recorder.pending, event.CallID)
			recorder.countTerminal(event.Phase)
			recorder.inputTokens = saturatingAdd(
				recorder.inputTokens, event.Usage.InputTokens,
			)
			if event.Usage.OutputTokens > 0 {
				recorder.outputTokens = saturatingAdd(
					recorder.outputTokens, event.Usage.OutputTokens,
				)
			} else {
				recorder.outputTokens = saturatingAdd(
					recorder.outputTokens, started.MaxOutputTokens,
				)
			}
		default:
			return fmt.Errorf("model receipt event %d phase is invalid", expectedSequence)
		}
		recorder.sequence = event.Sequence
		recorder.lastDigest = event.Digest
		lastDigest = event.Digest
		expectedSequence++
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan model receipt ledger: %w", err)
	}
	_, err := recorder.file.Seek(0, 2)
	return err
}

func (recorder *Recorder) countTerminal(phase string) {
	switch phase {
	case CallCompleted:
		recorder.completed++
	case CallFailed:
		recorder.failed++
	case CallUnknown:
		recorder.unknown++
	}
}

func validUsage(usage Usage, completed bool) bool {
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.TotalTokens < 0 {
		return false
	}
	if usage.TotalTokens != 0 {
		if usage.InputTokens > usage.TotalTokens ||
			usage.OutputTokens >
				usage.TotalTokens-usage.InputTokens {
			return false
		}
	}
	if completed && usage.InputTokens == 0 {
		return false
	}
	return true
}

func saturatingAdd(left, right int64) int64 {
	const maxInt64 = int64(^uint64(0) >> 1)
	if left < 0 || right < 0 || left > maxInt64-right {
		return maxInt64
	}
	return left + right
}

func (recorder *Recorder) pendingCalls() []CallEvent {
	result := make([]CallEvent, 0, len(recorder.pending))
	for _, event := range recorder.pending {
		result = append(result, event)
	}
	// Call IDs end in the start event sequence, so lexical ordering is not
	// sufficient after sequence 9. The ledger currently permits only bounded
	// pending concurrency and the recovery outcome is independent of order.
	return result
}

func computeEventDigest(event CallEvent) (string, error) {
	event.Digest = ""
	data, err := json.Marshal(event)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func shortDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}
