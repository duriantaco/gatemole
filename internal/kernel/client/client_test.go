package client

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
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
