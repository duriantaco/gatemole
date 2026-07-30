package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/duriantaco/vouch/internal/kernel/identity"
	"github.com/duriantaco/vouch/internal/kernel/model"
)

type authenticatedIdentityKey struct{}

func (s *Server) identityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			writeIdentityError(w, http.StatusUnauthorized, "missing bearer identity token")
			return
		}
		authenticated, err := s.identityVerifier.Verify(token, s.now().UTC())
		if err != nil {
			writeIdentityError(w, http.StatusUnauthorized, "identity token verification failed")
			return
		}
		requiredRole := "viewer"
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			requiredRole = "operator"
			if strings.HasSuffix(r.URL.Path, "/approvals") {
				requiredRole = "approver"
			}
		}
		if !authenticated.HasRole(requiredRole) {
			writeIdentityError(w, http.StatusForbidden, "identity role does not authorize this operation")
			return
		}
		namespace := namespaceFromPath(r.URL.EscapedPath())
		var body []byte
		if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
			body, err = io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
			if err != nil || len(body) > maxRequestBytes {
				writeIdentityError(w, http.StatusBadRequest, "identity middleware could not read the bounded request")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			if len(bytes.TrimSpace(body)) > 0 {
				var object map[string]json.RawMessage
				if err := json.Unmarshal(body, &object); err != nil {
					writeIdentityError(w, http.StatusBadRequest, "request body is not a JSON object")
					return
				}
				if namespace == "" {
					namespace = creationNamespace(object)
				}
				if actor, location, exists, err := requestActor(object); err != nil {
					writeIdentityError(w, http.StatusBadRequest, "request actor is invalid")
					return
				} else if exists &&
					(actor.ID != authenticated.Principal.ID ||
						actor.Kind != authenticated.Principal.Kind) {
					writeIdentityError(w, http.StatusForbidden, "request actor does not match the authenticated identity")
					return
				} else if exists &&
					(actor.Issuer != "" && actor.Issuer != authenticated.Issuer ||
						actor.ClaimsDigest != "" &&
							actor.ClaimsDigest != authenticated.Principal.ClaimsDigest) {
					writeIdentityError(w, http.StatusForbidden, "request actor contains unverified identity claims")
					return
				} else if exists && location == actorDecisionApprover &&
					actor.Issuer != authenticated.Issuer {
					writeIdentityError(w, http.StatusForbidden, "signed approval identity is not bound to the authenticated issuer")
					return
				} else if exists && location != actorDecisionApprover {
					body, err = bindAuthenticatedActor(
						r.URL.Path, object, location, authenticated.Principal,
					)
					if err != nil {
						writeIdentityError(w, http.StatusBadRequest, "request actor could not be bound to the authenticated identity")
						return
					}
					r.Body = io.NopCloser(bytes.NewReader(body))
					r.ContentLength = int64(len(body))
				}
			}
		}
		if namespace == "" || !authenticated.AllowsNamespace(namespace) {
			writeIdentityError(w, http.StatusForbidden, "identity does not authorize the request namespace")
			return
		}
		ctx := context.WithValue(r.Context(), authenticatedIdentityKey{}, authenticated)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func authenticatedIdentity(r *http.Request) (identity.Identity, bool) {
	value, ok := r.Context().Value(authenticatedIdentityKey{}).(identity.Identity)
	return value, ok
}

func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	token := strings.TrimPrefix(header, prefix)
	return token, token != "" && !strings.ContainsAny(token, " \t\r\n\x00")
}

func namespaceFromPath(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for index := 0; index+1 < len(parts); index++ {
		if parts[index] != "namespaces" {
			continue
		}
		namespace, err := url.PathUnescape(parts[index+1])
		if err == nil {
			return namespace
		}
		return ""
	}
	return ""
}

func creationNamespace(object map[string]json.RawMessage) string {
	payload := object["payload"]
	if len(payload) == 0 {
		return ""
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(payload, &envelope) != nil {
		return ""
	}
	for _, resourceName := range []string{"run", "transaction"} {
		var resource struct {
			Namespace string `json:"namespace"`
		}
		if raw := envelope[resourceName]; len(raw) > 0 &&
			json.Unmarshal(raw, &resource) == nil {
			return resource.Namespace
		}
	}
	return ""
}

type actorLocation string

const (
	actorTopLevel         actorLocation = "top_level"
	actorNestedEvent      actorLocation = "event"
	actorDecisionApprover actorLocation = "decision"
)

func requestActor(object map[string]json.RawMessage) (model.Principal, actorLocation, bool, error) {
	for _, candidate := range []struct {
		container string
		field     string
		location  actorLocation
	}{
		{"", "actor", actorTopLevel},
		{"event", "actor", actorNestedEvent},
		{"decision", "approver", actorDecisionApprover},
	} {
		container := object
		if candidate.container != "" {
			var nested map[string]json.RawMessage
			raw, exists := object[candidate.container]
			if !exists {
				continue
			}
			if err := json.Unmarshal(raw, &nested); err != nil {
				return model.Principal{}, "", false, err
			}
			container = nested
		}
		raw, exists := container[candidate.field]
		if !exists {
			continue
		}
		var actor model.Principal
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&actor); err != nil {
			return model.Principal{}, "", false, err
		}
		if !model.IsIdentifier(actor.ID) ||
			(actor.Kind != model.PrincipalHuman &&
				actor.Kind != model.PrincipalService &&
				actor.Kind != model.PrincipalOperator &&
				actor.Kind != model.PrincipalAgent &&
				actor.Kind != model.PrincipalRun) {
			return model.Principal{}, "", false, errors.New("actor principal is invalid")
		}
		return actor, candidate.location, true, nil
	}
	return model.Principal{}, "", false, nil
}

func bindAuthenticatedActor(
	path string,
	object map[string]json.RawMessage,
	location actorLocation,
	principal model.Principal,
) ([]byte, error) {
	principalData, err := json.Marshal(principal)
	if err != nil {
		return nil, err
	}
	switch location {
	case actorTopLevel:
		object["actor"] = principalData
		switch path {
		case "/v0/runs":
			data, err := json.Marshal(object)
			if err != nil {
				return nil, err
			}
			var event model.RunEvent
			if err := json.Unmarshal(data, &event); err != nil {
				return nil, err
			}
			event.Actor = principal
			event.Digest, err = model.ComputeEventDigest(event)
			if err != nil {
				return nil, err
			}
			object["digest"], err = json.Marshal(event.Digest)
			if err != nil {
				return nil, err
			}
		case "/v0/transactions":
			data, err := json.Marshal(object)
			if err != nil {
				return nil, err
			}
			var event model.TransactionEvent
			if err := json.Unmarshal(data, &event); err != nil {
				return nil, err
			}
			event.Actor = principal
			event.Digest, err = model.ComputeTransactionEventDigest(event)
			if err != nil {
				return nil, err
			}
			object["digest"], err = json.Marshal(event.Digest)
			if err != nil {
				return nil, err
			}
		}
	case actorNestedEvent:
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(object["event"], &nested); err != nil {
			return nil, err
		}
		nested["actor"] = principalData
		data, err := json.Marshal(nested)
		if err != nil {
			return nil, err
		}
		var event model.RunEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return nil, err
		}
		event.Actor = principal
		event.Digest, err = model.ComputeEventDigest(event)
		if err != nil {
			return nil, err
		}
		nested["digest"], err = json.Marshal(event.Digest)
		if err != nil {
			return nil, err
		}
		object["event"], err = json.Marshal(nested)
		if err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("actor location cannot be bound")
	}
	return json.Marshal(object)
}

func writeIdentityError(w http.ResponseWriter, status int, message string) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="gatemoled"`)
	}
	writeJSON(w, status, &model.KernelError{
		Code: model.ErrorIdentityInvalid, Operation: "authenticate_request",
		Message: message,
	})
}
