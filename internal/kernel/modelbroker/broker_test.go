package modelbroker

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testAgentToken    = "transaction-scoped-agent-token-000000000000000000"
	testProviderToken = "provider-secret-that-must-not-reach-the-agent"
)

func TestBrokerEnforcesPolicyReplacesCredentialAndRecordsDigests(t *testing.T) {
	var upstreamCalls atomic.Int64
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		upstreamCalls.Add(1)
		if r.URL.Path != "/v1/responses" {
			t.Errorf("upstream path=%q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testProviderToken {
			t.Errorf("upstream credential=%q", got)
		}
		if !strings.HasPrefix(r.Header.Get("X-Client-Request-Id"), "model-call:") {
			t.Errorf("missing Vouch request ID: %q", r.Header.Get("X-Client-Request-Id"))
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		var request map[string]any
		if err := json.Unmarshal(data, &request); err != nil {
			t.Error(err)
		}
		if request["store"] != false {
			t.Errorf("store was not forced false: %#v", request)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"application/json"},
				"X-Request-Id": []string{"req_provider_123"},
			},
			Body: io.NopCloser(strings.NewReader(`{
			"id":"resp_test",
			"usage":{"input_tokens":12,"output_tokens":7,"total_tokens":19},
			"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]
		}`)),
		}, nil
	})

	handler, receiptPath := testBroker(t, transport, 2)
	requestBody := `{
		"model":"gpt-test-2026-07-23",
		"input":"top secret prompt that cannot enter receipts",
		"max_output_tokens":20,
		"store":true,
		"tools":[{"type":"function","name":"read_file"}]
	}`
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(requestBody))
	request.Header.Set("Authorization", "Bearer "+testAgentToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("X-Request-Id") != "req_provider_123" {
		t.Fatalf("provider request ID was not relayed: %q", response.Header().Get("X-Request-Id"))
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("upstream calls=%d, want 1", upstreamCalls.Load())
	}
	events := readReceiptEvents(t, receiptPath)
	if len(events) != 2 || events[0].Phase != CallStarted || events[1].Phase != CallCompleted {
		t.Fatalf("unexpected receipt events: %#v", events)
	}
	if events[1].Usage.InputTokens != 12 || events[1].Usage.OutputTokens != 7 ||
		events[1].ProviderRequestID != "req_provider_123" {
		t.Fatalf("receipt lacks provider evidence: %#v", events[1])
	}
	if events[1].PreviousEventDigest != events[0].Digest {
		t.Fatal("receipt events are not hash chained")
	}
	ledger, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ledger, []byte("top secret prompt")) ||
		bytes.Contains(ledger, []byte(testProviderToken)) {
		t.Fatalf("receipt leaked prompt or provider credential: %s", ledger)
	}
}

func TestBrokerDeniesUnauthorizedModelsToolsStateAndBudgetsBeforeUpstream(t *testing.T) {
	var upstreamCalls atomic.Int64
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		upstreamCalls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`,
			)),
		}, nil
	})
	handler, receiptPath := testBroker(t, transport, 1)

	tests := []struct {
		name          string
		authorization string
		body          string
		wantStatus    int
	}{
		{"token", "Bearer wrong", `{"model":"gpt-test","input":"x","max_output_tokens":10}`, http.StatusUnauthorized},
		{"model", "Bearer " + testAgentToken, `{"model":"untrusted","input":"x","max_output_tokens":10}`, http.StatusForbidden},
		{"tool", "Bearer " + testAgentToken, `{"model":"gpt-test","input":"x","max_output_tokens":10,"tools":[{"type":"web_search"}]}`, http.StatusForbidden},
		{"state", "Bearer " + testAgentToken, `{"model":"gpt-test","input":"x","max_output_tokens":10,"previous_response_id":"resp_1"}`, http.StatusForbidden},
		{"missing output cap", "Bearer " + testAgentToken, `{"model":"gpt-test","input":"x"}`, http.StatusForbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(test.body))
			request.Header.Set("Authorization", test.authorization)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d, want %d: %s", response.Code, test.wantStatus, response.Body.String())
			}
		})
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("denied request reached upstream %d time(s)", upstreamCalls.Load())
	}

	allowed := httptest.NewRequest(
		http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"gpt-test","input":"x","max_output_tokens":10}`),
	)
	allowed.Header.Set("Authorization", "Bearer "+testAgentToken)
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, allowed)
	if first.Code != http.StatusOK {
		t.Fatalf("allowed request status=%d: %s", first.Code, first.Body.String())
	}
	exhausted := httptest.NewRequest(
		http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"gpt-test","input":"x","max_output_tokens":10}`),
	)
	exhausted.Header.Set("Authorization", "Bearer "+testAgentToken)
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, exhausted)
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("budget status=%d, want 429: %s", second.Code, second.Body.String())
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("upstream calls=%d, want 1", upstreamCalls.Load())
	}
	if events := readReceiptEvents(t, receiptPath); len(events) != 2 {
		t.Fatalf("denied requests changed receipt ledger: %#v", events)
	}
}

func TestBrokerReservesInputBudgetBeforeCallingProvider(t *testing.T) {
	var upstreamCalls atomic.Int64
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		upstreamCalls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`,
			)),
		}, nil
	})
	handler, receiptPath := testBroker(t, transport, 2)
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(
			`{"model":"gpt-test","input":"`+
				strings.Repeat("x", 1100)+
				`","max_output_tokens":10}`,
		),
	)
	request.Header.Set("Authorization", "Bearer "+testAgentToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf(
			"oversized input reservation status=%d, want 429: %s",
			response.Code,
			response.Body.String(),
		)
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("input-budget violation reached upstream %d time(s)", upstreamCalls.Load())
	}
	if events := readReceiptEvents(t, receiptPath); len(events) != 0 {
		t.Fatalf("input-budget violation changed receipt ledger: %#v", events)
	}
}

func TestBrokerFailsClosedOnOverflowingProviderUsage(t *testing.T) {
	const maxInt64 = int64(^uint64(0) >> 1)
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"application/json"},
			},
			Body: io.NopCloser(strings.NewReader(
				`{"usage":{"input_tokens":9223372036854775807,` +
					`"output_tokens":9223372036854775807,` +
					`"total_tokens":9223372036854775807}}`,
			)),
		}, nil
	})
	handler, receiptPath := testBroker(t, transport, 2)
	call := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(
			http.MethodPost,
			"/v1/responses",
			strings.NewReader(
				`{"model":"gpt-test","input":"x","max_output_tokens":10}`,
			),
		)
		request.Header.Set("Authorization", "Bearer "+testAgentToken)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	if response := call(); response.Code != http.StatusOK {
		t.Fatalf("provider response status=%d: %s", response.Code, response.Body.String())
	}
	if response := call(); response.Code != http.StatusTooManyRequests {
		t.Fatalf("overflowing usage reopened budget: status=%d", response.Code)
	}
	events := readReceiptEvents(t, receiptPath)
	if len(events) != 2 ||
		events[1].Phase != CallUnknown ||
		events[1].Usage.InputTokens != 1000 ||
		!events[1].Usage.Estimated {
		t.Fatalf("overflowing usage was not failed closed: %#v", events)
	}
	if validUsage(Usage{
		InputTokens: maxInt64, OutputTokens: maxInt64,
		TotalTokens: maxInt64,
	}, true) {
		t.Fatal("overflowing token total was accepted")
	}
}

func TestConcurrentReservationsCannotOvercommitInputBudget(t *testing.T) {
	broker := &Broker{policy: Policy{
		MaxRequests:          2,
		MaxTotalInputTokens:  100,
		MaxTotalOutputTokens: 100,
	}}
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			results <- broker.reserve(60, 1)
		}()
	}
	close(start)
	successes := 0
	for range 2 {
		if err := <-results; err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful reservations=%d, want exactly 1", successes)
	}
}

func TestRecorderReopensChainAndClosesInterruptedCallAsUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model-calls.jsonl")
	recorder, err := OpenRecorder(path, "tx:restart", "run:restart", "openai")
	if err != nil {
		t.Fatal(err)
	}
	started, err := recorder.Start(
		"gpt-test", digestBytes([]byte("request")), 25,
		time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if started.Phase != CallStarted {
		t.Fatalf("start=%#v", started)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenRecorder(path, "tx:restart", "run:restart", "openai")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	snapshot := reopened.BudgetSnapshot()
	if snapshot.Requests != 1 || snapshot.OutputTokens != 25 || snapshot.Unknown != 1 {
		t.Fatalf("recovered budget=%#v", snapshot)
	}
	events := readReceiptEvents(t, path)
	if len(events) != 2 || events[1].Phase != CallUnknown ||
		events[1].ErrorCode != "broker_restarted" {
		t.Fatalf("interrupted call was not closed unknown: %#v", events)
	}
}

func TestBrokerFailsClosedWhenProviderUsageIsMissing(t *testing.T) {
	var upstreamCalls atomic.Int64
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		upstreamCalls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"resp_without_usage"}`)),
		}, nil
	})
	handler, receiptPath := testBroker(t, transport, 2)
	call := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(
			http.MethodPost, "/v1/responses",
			strings.NewReader(`{"model":"gpt-test","input":"x","max_output_tokens":10}`),
		)
		request.Header.Set("Authorization", "Bearer "+testAgentToken)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	if response := call(); response.Code != http.StatusOK {
		t.Fatalf("provider response status=%d", response.Code)
	}
	if response := call(); response.Code != http.StatusTooManyRequests {
		t.Fatalf("missing usage did not exhaust budget: status=%d", response.Code)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("missing usage reached upstream %d times, want 1", upstreamCalls.Load())
	}
	events := readReceiptEvents(t, receiptPath)
	if len(events) != 2 || events[1].Phase != CallUnknown ||
		events[1].ErrorCode != "provider_usage_unavailable" ||
		!events[1].Usage.Estimated ||
		events[1].Usage.InputTokens != 1000 {
		t.Fatalf("missing usage was not conservatively receipted: %#v", events)
	}
}

func TestBrokerNeverWritesBeyondResponseByteLimit(t *testing.T) {
	const responseLimit = int64(1024)
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(strings.Repeat("x", int(responseLimit+50)))),
		}, nil
	})
	policy := Policy{
		Version: PolicyVersion, Provider: "openai",
		UpstreamBaseURL: "http://provider.invalid",
		AllowedModels:   []string{"gpt-test"},
		MaxRequests:     1, MaxRequestBytes: 1 << 20, MaxResponseBytes: responseLimit,
		MaxOutputTokensPerRequest: 100, MaxTotalInputTokens: 1000,
		MaxTotalOutputTokens: 1000, RequestTimeoutSeconds: 10, ForceStoreFalse: true,
	}
	receiptPath := filepath.Join(t.TempDir(), "model-calls.jsonl")
	recorder, err := OpenRecorder(
		receiptPath, "tx:bounded-response", "run:bounded-response", policy.Provider,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	broker, err := New(Config{
		Policy: policy, TransactionID: "tx:bounded-response", RunID: "run:bounded-response",
		AgentToken: testAgentToken, ProviderBearerToken: testProviderToken,
		Recorder: recorder, Transport: transport,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"gpt-test","input":"x","max_output_tokens":10}`),
	)
	request.Header.Set("Authorization", "Bearer "+testAgentToken)
	response := httptest.NewRecorder()
	broker.Handler().ServeHTTP(response, request)
	if int64(response.Body.Len()) != responseLimit {
		t.Fatalf("response bytes=%d, want exact cap %d", response.Body.Len(), responseLimit)
	}
	events := readReceiptEvents(t, receiptPath)
	if len(events) != 2 || events[1].Phase != CallUnknown ||
		events[1].ErrorCode != "response_too_large" {
		t.Fatalf("oversized response was not recorded unknown: %#v", events)
	}
}

func testBroker(t *testing.T, transport http.RoundTripper, maxRequests int64) (http.Handler, string) {
	t.Helper()
	policy := Policy{
		Version: PolicyVersion, Provider: "openai", UpstreamBaseURL: "http://provider.invalid",
		AllowedModels: []string{"gpt-test*"}, AllowedToolTypes: []string{"function"},
		MaxRequests: maxRequests, MaxRequestBytes: 1 << 20, MaxResponseBytes: 1 << 20,
		MaxOutputTokensPerRequest: 100, MaxTotalInputTokens: 1000,
		MaxTotalOutputTokens: 1000, RequestTimeoutSeconds: 10, ForceStoreFalse: true,
	}
	receiptPath := filepath.Join(t.TempDir(), "model-calls.jsonl")
	recorder, err := OpenRecorder(receiptPath, "tx:broker-test", "run:broker-test", policy.Provider)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	broker, err := New(Config{
		Policy: policy, TransactionID: "tx:broker-test", RunID: "run:broker-test",
		AgentToken: testAgentToken, ProviderBearerToken: testProviderToken,
		Recorder: recorder, Logger: log.New(io.Discard, "", 0), Transport: transport,
	})
	if err != nil {
		t.Fatal(err)
	}
	return broker.Handler(), receiptPath
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func readReceiptEvents(t *testing.T, path string) []CallEvent {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var events []CallEvent
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var event CallEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}
