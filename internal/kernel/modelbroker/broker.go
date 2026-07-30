package modelbroker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Policy              Policy
	Production          bool
	TransactionID       string
	RunID               string
	AgentToken          string
	ProviderBearerToken string
	Recorder            *Recorder
	Logger              *log.Logger
	Transport           http.RoundTripper
	Now                 func() time.Time
}

type Broker struct {
	policy              Policy
	transactionID       string
	runID               string
	agentToken          string
	providerBearerToken string
	recorder            *Recorder
	logger              *log.Logger
	client              *http.Client
	now                 func() time.Time

	mu             sync.Mutex
	requests       int64
	inputTokens    int64
	outputTokens   int64
	reservedInput  int64
	reservedOutput int64
}

type responseUsage struct {
	Usage Usage `json:"usage"`
}

func New(config Config) (*Broker, error) {
	if err := config.Policy.Validate(config.Production); err != nil {
		return nil, err
	}
	if !identifier(config.TransactionID) || !identifier(config.RunID) {
		return nil, errors.New("model broker transaction and run IDs are invalid")
	}
	if len(config.AgentToken) < 32 || strings.ContainsAny(config.AgentToken, "\r\n\x00") {
		return nil, errors.New("model broker agent token must contain at least 32 safe bytes")
	}
	if strings.TrimSpace(config.ProviderBearerToken) == "" ||
		strings.ContainsAny(config.ProviderBearerToken, "\r\n\x00") {
		return nil, errors.New("model broker provider credential is invalid")
	}
	if config.Recorder == nil {
		return nil, errors.New("model broker receipt recorder is required")
	}
	logger := config.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	transport := config.Transport
	if transport == nil {
		transport = &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
			},
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          16,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		}
	}
	snapshot := config.Recorder.BudgetSnapshot()
	inputTokens := snapshot.InputTokens
	if snapshot.Unknown > 0 {
		inputTokens = config.Policy.MaxTotalInputTokens
	}
	return &Broker{
		policy: config.Policy, transactionID: config.TransactionID, runID: config.RunID,
		agentToken: config.AgentToken, providerBearerToken: config.ProviderBearerToken,
		recorder: config.Recorder, logger: logger, now: now,
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("model provider redirects are disabled")
			},
		},
		requests: snapshot.Requests, inputTokens: inputTokens,
		outputTokens: snapshot.OutputTokens,
	}, nil
}

func (broker *Broker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", broker.health)
	mux.HandleFunc("POST /v1/responses", broker.responses)
	return mux
}

func (broker *Broker) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")
}

func (broker *Broker) responses(w http.ResponseWriter, request *http.Request) {
	if !broker.authorized(request.Header.Get("Authorization")) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeBrokerError(w, http.StatusUnauthorized, "unauthorized", "invalid transaction-scoped broker token")
		return
	}
	body, err := readBounded(request.Body, broker.policy.MaxRequestBytes)
	if err != nil {
		writeBrokerError(w, http.StatusRequestEntityTooLarge, "request_too_large", err.Error())
		return
	}
	prepared, err := broker.prepareRequest(body)
	if err != nil {
		writeBrokerError(w, http.StatusForbidden, "policy_denied", err.Error())
		return
	}
	if err := broker.reserve(
		prepared.InputTokenReservation,
		prepared.MaxOutputTokens,
	); err != nil {
		writeBrokerError(w, http.StatusTooManyRequests, "budget_exceeded", err.Error())
		return
	}
	requestDigest := digestBytes(prepared.Body)
	started, err := broker.recorder.Start(
		prepared.Model, requestDigest, prepared.MaxOutputTokens, broker.now().UTC(),
	)
	if err != nil {
		broker.releaseReservation(
			prepared.InputTokenReservation,
			prepared.MaxOutputTokens,
			Usage{},
		)
		writeBrokerError(w, http.StatusServiceUnavailable, "receipt_unavailable", "cannot durably record model call")
		return
	}
	completed := CallEvent{OccurredAt: broker.now().UTC()}
	phase := CallUnknown
	defer func() {
		if completed.Usage.InputTokens == 0 ||
			!validUsage(completed.Usage, phase == CallCompleted) ||
			completed.Usage.InputTokens >
				broker.policy.MaxTotalInputTokens ||
			completed.Usage.OutputTokens >
				broker.policy.MaxTotalOutputTokens {
			completed.Usage = Usage{
				InputTokens:  broker.policy.MaxTotalInputTokens,
				OutputTokens: prepared.MaxOutputTokens,
				TotalTokens: broker.policy.MaxTotalInputTokens +
					prepared.MaxOutputTokens,
				Estimated: true,
			}
			if phase == CallCompleted {
				phase = CallUnknown
				completed.ErrorCode = "provider_usage_unavailable"
			}
		}
		if _, recordErr := broker.recorder.Finish(started, phase, completed); recordErr != nil {
			broker.logger.Printf("model broker receipt failure call=%s error=%v", started.CallID, recordErr)
		}
		broker.releaseReservation(
			prepared.InputTokenReservation,
			prepared.MaxOutputTokens,
			completed.Usage,
		)
	}()

	upstreamURL, _ := url.JoinPath(broker.policy.UpstreamBaseURL, "v1", "responses")
	timeoutContext, cancel := context.WithTimeout(
		request.Context(),
		time.Duration(broker.policy.RequestTimeoutSeconds)*time.Second,
	)
	defer cancel()
	upstreamRequest, err := http.NewRequestWithContext(
		timeoutContext, http.MethodPost, upstreamURL, bytes.NewReader(prepared.Body),
	)
	if err != nil {
		completed.ErrorCode = "request_build_failed"
		phase = CallFailed
		writeBrokerError(w, http.StatusInternalServerError, completed.ErrorCode, "cannot construct provider request")
		return
	}
	upstreamRequest.Header.Set("Authorization", "Bearer "+broker.providerBearerToken)
	upstreamRequest.Header.Set("Content-Type", "application/json")
	upstreamRequest.Header.Set("Accept", request.Header.Get("Accept"))
	upstreamRequest.Header.Set("X-Client-Request-Id", started.CallID)
	for name, value := range broker.policy.ProviderHeaders {
		upstreamRequest.Header.Set(name, value)
	}
	response, err := broker.client.Do(upstreamRequest)
	if err != nil {
		completed.ErrorCode = "provider_unavailable"
		phase = CallUnknown
		writeBrokerError(w, http.StatusBadGateway, completed.ErrorCode, "model provider request failed")
		return
	}
	defer response.Body.Close()
	completed.HTTPStatus = response.StatusCode
	completed.ProviderRequestID = cleanRequestID(response.Header.Get("X-Request-Id"))
	copyResponseHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	responseBody, responseDigest, copyErr := copyBoundedResponse(
		w, response.Body, broker.policy.MaxResponseBytes,
	)
	completed.ResponseDigest = responseDigest
	if copyErr != nil {
		completed.ErrorCode = "response_too_large"
		phase = CallUnknown
		return
	}
	usage, usageOK := extractUsage(responseBody, response.Header.Get("Content-Type"))
	completed.Usage = usage
	if !usageOK {
		completed.ErrorCode = "provider_usage_unavailable"
		phase = CallUnknown
		return
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		phase = CallCompleted
	} else {
		completed.ErrorCode = "provider_http_" + strconv.Itoa(response.StatusCode)
		phase = CallFailed
	}
}

type preparedRequest struct {
	Body            []byte
	Model           string
	MaxOutputTokens int64
	// UTF-8 byte length is a conservative upper bound for provider tokenizers.
	// Reserving it closes the race where concurrent requests could all enter
	// before provider-reported usage updated the durable budget.
	InputTokenReservation int64
}

func (broker *Broker) prepareRequest(body []byte) (preparedRequest, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var object map[string]json.RawMessage
	if err := decoder.Decode(&object); err != nil {
		return preparedRequest{}, errors.New("request must be one JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return preparedRequest{}, errors.New("request contains trailing JSON")
	}
	var model string
	if err := json.Unmarshal(object["model"], &model); err != nil ||
		!broker.policy.AllowsModel(model) {
		return preparedRequest{}, errors.New("requested model is not allowed")
	}
	var maxOutput int64
	if err := json.Unmarshal(object["max_output_tokens"], &maxOutput); err != nil ||
		maxOutput < 1 || maxOutput > broker.policy.MaxOutputTokensPerRequest {
		return preparedRequest{}, errors.New("max_output_tokens is required and exceeds policy")
	}
	if !broker.policy.AllowStatefulRequests {
		for _, field := range []string{"background", "conversation", "previous_response_id"} {
			if raw, exists := object[field]; exists && string(raw) != "null" && string(raw) != "false" && string(raw) != `""` {
				return preparedRequest{}, fmt.Errorf("%s is disabled by broker policy", field)
			}
		}
	}
	if raw, exists := object["tools"]; exists {
		var tools []struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &tools); err != nil {
			return preparedRequest{}, errors.New("tools must be an array")
		}
		for _, tool := range tools {
			if !broker.policy.AllowsTool(tool.Type) {
				return preparedRequest{}, fmt.Errorf("tool type %q is not allowed", tool.Type)
			}
		}
	}
	if broker.policy.ForceStoreFalse {
		object["store"] = json.RawMessage("false")
	}
	preparedBody, err := json.Marshal(object)
	if err != nil {
		return preparedRequest{}, errors.New("cannot normalize provider request")
	}
	return preparedRequest{
		Body:                  preparedBody,
		Model:                 model,
		MaxOutputTokens:       maxOutput,
		InputTokenReservation: int64(len(preparedBody)),
	}, nil
}

func (broker *Broker) reserve(inputReservation, maxOutput int64) error {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if broker.requests >= broker.policy.MaxRequests {
		return errors.New("model request budget exhausted")
	}
	if inputReservation < 1 ||
		wouldExceedBudget(
			broker.inputTokens,
			broker.reservedInput,
			inputReservation,
			broker.policy.MaxTotalInputTokens,
		) {
		return errors.New("model input token budget exhausted")
	}
	if wouldExceedBudget(
		broker.outputTokens,
		broker.reservedOutput,
		maxOutput,
		broker.policy.MaxTotalOutputTokens,
	) {
		return errors.New("model output token budget exhausted")
	}
	broker.requests++
	broker.reservedInput += inputReservation
	broker.reservedOutput += maxOutput
	return nil
}

func (broker *Broker) releaseReservation(
	inputReservation, maxOutput int64,
	usage Usage,
) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.reservedInput -= inputReservation
	if broker.reservedInput < 0 {
		broker.reservedInput = 0
	}
	broker.reservedOutput -= maxOutput
	if broker.reservedOutput < 0 {
		broker.reservedOutput = 0
	}
	broker.inputTokens = saturatingAdd(
		broker.inputTokens, usage.InputTokens,
	)
	if usage.OutputTokens > 0 {
		broker.outputTokens = saturatingAdd(
			broker.outputTokens, usage.OutputTokens,
		)
	} else {
		broker.outputTokens = saturatingAdd(
			broker.outputTokens, maxOutput,
		)
	}
}

func wouldExceedBudget(current, reserved, requested, limit int64) bool {
	if current < 0 || reserved < 0 || requested < 0 || limit < 0 ||
		current > limit {
		return true
	}
	remaining := limit - current
	if reserved > remaining {
		return true
	}
	return requested > remaining-reserved
}

func (broker *Broker) authorized(header string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	provided := []byte(strings.TrimPrefix(header, prefix))
	expected := []byte(broker.agentToken)
	return len(provided) == len(expected) &&
		subtle.ConstantTimeCompare(provided, expected) == 1
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("request exceeds broker byte limit")
	}
	return data, nil
}

func copyBoundedResponse(
	writer http.ResponseWriter,
	reader io.Reader,
	limit int64,
) ([]byte, string, error) {
	hasher := sha256.New()
	var captured bytes.Buffer
	target := io.MultiWriter(writer, hasher, &captured)
	written, err := io.Copy(target, io.LimitReader(reader, limit))
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
	digest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if err != nil {
		return captured.Bytes(), digest, err
	}
	if written == limit {
		var probe [1]byte
		probeBytes, probeErr := reader.Read(probe[:])
		if probeBytes > 0 {
			return captured.Bytes(), digest, errors.New("provider response exceeds broker byte limit")
		}
		if probeErr != nil && !errors.Is(probeErr, io.EOF) {
			return captured.Bytes(), digest, probeErr
		}
	}
	return captured.Bytes(), digest, nil
}

func extractUsage(body []byte, contentType string) (Usage, bool) {
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		scanner := bufio.NewScanner(bytes.NewReader(body))
		scanner.Buffer(make([]byte, 64<<10), 2<<20)
		var usage Usage
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var envelope struct {
				Type     string        `json:"type"`
				Response responseUsage `json:"response"`
			}
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &envelope) == nil &&
				envelope.Type == "response.completed" {
				usage = envelope.Response.Usage
			}
		}
		return usage, validUsage(usage, true)
	}
	var response responseUsage
	if json.Unmarshal(body, &response) != nil {
		return Usage{}, false
	}
	return response.Usage, validUsage(response.Usage, true)
}

func copyResponseHeaders(target, source http.Header) {
	for _, name := range []string{
		"Content-Type", "OpenAI-Organization", "OpenAI-Processing-Ms",
		"OpenAI-Version", "X-Request-Id",
		"X-Ratelimit-Limit-Requests", "X-Ratelimit-Limit-Tokens",
		"X-Ratelimit-Remaining-Requests", "X-Ratelimit-Remaining-Tokens",
		"X-Ratelimit-Reset-Requests", "X-Ratelimit-Reset-Tokens",
	} {
		if value := source.Get(name); value != "" && !strings.ContainsAny(value, "\r\n\x00") {
			target.Set(name, value)
		}
	}
}

func cleanRequestID(value string) string {
	if len(value) > 512 || strings.ContainsAny(value, "\r\n\x00") {
		return ""
	}
	return value
}

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func writeBrokerError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"type":    "gatemole_model_broker_error",
			"code":    code,
			"message": message,
		},
	})
}
