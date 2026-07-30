package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/duriantaco/gatemole/internal/kernel/identity"
	"github.com/duriantaco/gatemole/internal/kernel/model"
	"github.com/duriantaco/gatemole/internal/kernel/runtimepreflight"
	"github.com/duriantaco/gatemole/internal/kernel/store"
)

func TestRuntimePreflightUsesDaemonPolicyAndExactImage(t *testing.T) {
	t.Parallel()
	runtimeID := "runtime:" + strings.Repeat("a", 64)
	imageDigest := testDigest("b")
	engine, logPath := writePreflightEngine(t, false)
	kernelStore, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	handler := NewServer(
		kernelStore,
		WithRuntimeIdentity(runtimeID),
		WithExecutionRuntimePolicy(ExecutionRuntimePolicy{
			EnforcementProfile: store.EnforcementProfileProduction,
			EnginePath:         engine,
			AllowedAgentImageDigests: map[string]struct{}{
				imageDigest: {},
			},
		}),
	).Handler()
	request := validRuntimePreflightRequest(runtimeID, imageDigest)
	request.RequiredEnforcementProfile =
		store.EnforcementProfileProduction

	response := requestJSON(
		t,
		handler,
		http.MethodPost,
		"/v0/namespaces/engineering/runtime/preflight",
		request,
	)
	if response.Code != http.StatusOK {
		t.Fatalf(
			"preflight status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}
	var result runtimepreflight.Result
	decodeResponse(t, response, &result)
	if err := result.Validate(); err != nil {
		t.Fatal(err)
	}
	if result.RuntimeID != runtimeID ||
		result.EnforcementProfile != store.EnforcementProfileProduction ||
		result.AgentProfileID != "agent-profile:doctor" ||
		result.ImageDigest != imageDigest {
		t.Fatalf("unexpected preflight result: %#v", result)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	logged := string(logData)
	if !strings.Contains(logged, "info --format") ||
		!strings.Contains(logged, "image inspect --format") ||
		!strings.Contains(
			logged,
			"registry.example.invalid/agent@"+imageDigest,
		) {
		t.Fatalf("daemon did not inspect the exact selected image:\n%s", logged)
	}
}

func TestRuntimePreflightRejectsEnforcementProfileDowngradeBeforeEngine(
	t *testing.T,
) {
	t.Parallel()
	runtimeID := "runtime:" + strings.Repeat("a", 64)
	engine, logPath := writePreflightEngine(t, false)
	kernelStore, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	handler := NewServer(
		kernelStore,
		WithRuntimeIdentity(runtimeID),
		WithExecutionRuntimePolicy(ExecutionRuntimePolicy{
			EnforcementProfile: store.EnforcementProfileDevelopment,
			EnginePath:         engine,
		}),
	).Handler()
	request := validRuntimePreflightRequest(runtimeID, testDigest("b"))
	request.RequiredEnforcementProfile =
		store.EnforcementProfileProduction

	response := requestJSON(
		t,
		handler,
		http.MethodPost,
		"/v0/namespaces/engineering/runtime/preflight",
		request,
	)
	if response.Code != http.StatusForbidden {
		t.Fatalf(
			"profile downgrade status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}
	assertEmptyPreflightLog(t, logPath)
}

func TestRuntimePreflightProductionRequiresAgentImageAllowlist(
	t *testing.T,
) {
	t.Parallel()
	runtimeID := "runtime:" + strings.Repeat("a", 64)
	engine, logPath := writePreflightEngine(t, false)
	kernelStore, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	handler := NewServer(
		kernelStore,
		WithRuntimeIdentity(runtimeID),
		WithExecutionRuntimePolicy(ExecutionRuntimePolicy{
			EnforcementProfile: store.EnforcementProfileProduction,
			EnginePath:         engine,
		}),
	).Handler()

	response := requestJSON(
		t,
		handler,
		http.MethodPost,
		"/v0/namespaces/engineering/runtime/preflight",
		validRuntimePreflightRequest(runtimeID, testDigest("b")),
	)
	if response.Code != http.StatusForbidden {
		t.Fatalf(
			"empty production allowlist status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}
	assertEmptyPreflightLog(t, logPath)
}

func TestRuntimePreflightRejectsWrongRuntimeBeforeEngine(t *testing.T) {
	t.Parallel()
	runtimeID := "runtime:" + strings.Repeat("a", 64)
	engine, logPath := writePreflightEngine(t, false)
	kernelStore, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	handler := NewServer(
		kernelStore,
		WithRuntimeIdentity(runtimeID),
		WithExecutionRuntimePolicy(ExecutionRuntimePolicy{
			EnginePath: engine,
		}),
	).Handler()
	request := validRuntimePreflightRequest(runtimeID, testDigest("b"))
	request.ExpectedRuntimeID =
		"runtime:" + strings.Repeat("f", 64)

	response := requestJSON(
		t,
		handler,
		http.MethodPost,
		"/v0/namespaces/engineering/runtime/preflight",
		request,
	)
	if response.Code != http.StatusForbidden {
		t.Fatalf(
			"wrong Runtime status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}
	assertEmptyPreflightLog(t, logPath)
}

func TestRuntimePreflightChecksPolicyBeforeImageExistence(t *testing.T) {
	t.Parallel()
	runtimeID := "runtime:" + strings.Repeat("a", 64)
	engine, logPath := writePreflightEngine(t, false)
	kernelStore, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	handler := NewServer(
		kernelStore,
		WithRuntimeIdentity(runtimeID),
		WithExecutionRuntimePolicy(ExecutionRuntimePolicy{
			EnginePath: engine,
			AllowedAgentImageDigests: map[string]struct{}{
				testDigest("c"): {},
			},
		}),
	).Handler()

	response := requestJSON(
		t,
		handler,
		http.MethodPost,
		"/v0/namespaces/engineering/runtime/preflight",
		validRuntimePreflightRequest(runtimeID, testDigest("b")),
	)
	if response.Code != http.StatusForbidden {
		t.Fatalf(
			"denied image status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}
	assertEmptyPreflightLog(t, logPath)
}

func TestRuntimePreflightHidesEngineFailureOutput(t *testing.T) {
	t.Parallel()
	runtimeID := "runtime:" + strings.Repeat("a", 64)
	engine, _ := writePreflightEngine(t, true)
	kernelStore, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	handler := NewServer(
		kernelStore,
		WithRuntimeIdentity(runtimeID),
		WithExecutionRuntimePolicy(ExecutionRuntimePolicy{
			EnginePath: engine,
		}),
	).Handler()

	response := requestJSON(
		t,
		handler,
		http.MethodPost,
		"/v0/namespaces/engineering/runtime/preflight",
		validRuntimePreflightRequest(runtimeID, testDigest("b")),
	)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf(
			"missing image status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}
	if strings.Contains(response.Body.String(), "engine-private-output") {
		t.Fatalf("engine output leaked through preflight: %s", response.Body.String())
	}
	var unavailable model.KernelError
	decodeResponse(t, response, &unavailable)
	if unavailable.Code != model.ErrorDriverUnavailable {
		t.Fatalf("preflight error=%q", unavailable.Code)
	}
}

func TestRuntimePreflightRequiresOperatorInExactNamespaceBeforeEngine(
	t *testing.T,
) {
	t.Parallel()
	now := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	jwk, err := identity.Ed25519JWK("key:preflight", publicKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := identity.NewVerifier(identity.TrustDocument{
		Version:                 identity.TrustDocumentVersion,
		Issuer:                  "https://issuer.example.invalid",
		Audiences:               []string{"gatemole-api"},
		MaxTokenLifetimeSeconds: 3600,
		JWKS:                    identity.JWKS{Keys: []identity.JWK{jwk}},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtimeID := "runtime:" + strings.Repeat("a", 64)
	imageDigest := testDigest("b")
	engine, logPath := writePreflightEngine(t, false)
	kernelStore, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	handler := NewServer(
		kernelStore,
		WithClock(func() time.Time { return now }),
		WithIdentityVerifier(verifier),
		WithRuntimeIdentity(runtimeID),
		WithExecutionRuntimePolicy(ExecutionRuntimePolicy{
			EnginePath: engine,
		}),
	).Handler()
	path := "/v0/namespaces/engineering/runtime/preflight"
	request := validRuntimePreflightRequest(runtimeID, imageDigest)
	viewerToken := apiIdentityToken(
		t,
		privateKey,
		"key:preflight",
		now,
		"service:viewer",
		model.PrincipalService,
		[]string{"engineering"},
		[]string{"viewer"},
	)
	wrongNamespaceToken := apiIdentityToken(
		t,
		privateKey,
		"key:preflight",
		now,
		"operator:wrong-namespace",
		model.PrincipalOperator,
		[]string{"finance"},
		[]string{"operator"},
	)
	operatorToken := apiIdentityToken(
		t,
		privateKey,
		"key:preflight",
		now,
		"operator:preflight",
		model.PrincipalOperator,
		[]string{"engineering"},
		[]string{"operator"},
	)

	for name, test := range map[string]struct {
		token string
		want  int
	}{
		"missing token":   {want: http.StatusUnauthorized},
		"viewer role":     {token: viewerToken, want: http.StatusForbidden},
		"wrong namespace": {token: wrongNamespaceToken, want: http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			response := requestJSONWithToken(
				t,
				handler,
				http.MethodPost,
				path,
				request,
				test.token,
			)
			if response.Code != test.want {
				t.Fatalf(
					"status=%d, want %d body=%s",
					response.Code,
					test.want,
					response.Body.String(),
				)
			}
			assertEmptyPreflightLog(t, logPath)
		})
	}

	response := requestJSONWithToken(
		t,
		handler,
		http.MethodPost,
		path,
		request,
		operatorToken,
	)
	if response.Code != http.StatusOK {
		t.Fatalf(
			"operator preflight status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}
}

func validRuntimePreflightRequest(
	runtimeID string,
	imageDigest string,
) runtimepreflight.Request {
	return runtimepreflight.Request{
		Version:           runtimepreflight.RequestVersion,
		ExpectedRuntimeID: runtimeID,
		Agent: &runtimepreflight.AgentSelection{
			Profile: model.AgentTaskProfileBinding{
				ID:            "agent-profile:doctor",
				Digest:        testDigest("a"),
				RuntimeClass:  "oci",
				ImageDigest:   imageDigest,
				Entrypoint:    "/opt/agent",
				CommandDigest: testDigest("c"),
			},
			OCIImage: "registry.example.invalid/agent@" + imageDigest,
		},
	}
}

func writePreflightEngine(
	t *testing.T,
	failImage bool,
) (string, string) {
	t.Helper()
	directory := t.TempDir()
	logPath := filepath.Join(directory, "engine.log")
	enginePath := filepath.Join(directory, "engine")
	fail := ""
	if failImage {
		fail = `
if [ "$1" = "image" ]; then
  echo "engine-private-output" >&2
  exit 1
fi
`
	}
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
` + fail + `
exit 0
`
	if err := os.WriteFile(enginePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return enginePath, logPath
}

func assertEmptyPreflightLog(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Fatalf("preflight invoked engine before authorization:\n%s", data)
	}
}
