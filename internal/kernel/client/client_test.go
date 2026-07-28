package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/duriantaco/vouch/internal/kernel/admission"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestClientSendsExplicitBearerToken(t *testing.T) {
	t.Parallel()
	var authorization string
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		authorization = request.Header.Get("Authorization")
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("[]")),
			Request:    request,
		}, nil
	})
	client := NewWithTransport(transport).WithBearerToken("signed-token")
	if _, err := client.ListRuns(context.Background(), "team-a"); err != nil {
		t.Fatal(err)
	}
	if authorization != "Bearer signed-token" {
		t.Fatalf("authorization=%q", authorization)
	}
}

func TestClientAdmitTaskUsesNamespaceEndpoint(t *testing.T) {
	t.Parallel()
	input := admission.Request{
		Version:        admission.RequestVersion,
		IdempotencyKey: "admission:client-test",
		TransactionID:  "tx:client-test",
		Intent:         "Exercise the task admission client.",
	}
	want := admission.Result{
		Version:        admission.ResultVersion,
		IdempotencyKey: input.IdempotencyKey,
		RequestDigest:  "sha256:" + strings.Repeat("a", 64),
	}
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost {
			t.Fatalf("method=%q", request.Method)
		}
		if request.URL.EscapedPath() !=
			"/v0/namespaces/team%2Fplatform/task-admissions" {
			t.Fatalf("path=%q", request.URL.EscapedPath())
		}
		var got admission.Request
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, input) {
			t.Fatalf("request=%#v, want %#v", got, input)
		}
		data, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{
			StatusCode: http.StatusCreated,
			Status:     "201 Created",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(string(data))),
			Request:    request,
		}, nil
	})
	client := NewWithTransport(transport)
	got, err := client.AdmitTask(
		context.Background(),
		"team/platform",
		input,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("result=%#v, want %#v", got, want)
	}
}
