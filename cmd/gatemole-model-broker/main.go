package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/modelbroker"
)

func main() {
	os.Exit(run())
}

func run() int {
	flags := flag.NewFlagSet("gatemole-model-broker", flag.ContinueOnError)
	listen := flags.String("listen", "0.0.0.0:8080", "broker listen address")
	policyPath := flags.String("policy", "", "model broker policy JSON")
	receiptsPath := flags.String("receipts", "", "append-only model receipt JSONL")
	readyPath := flags.String("ready-file", "", "write readiness marker after binding")
	transactionID := flags.String("transaction", "", "transaction ID")
	runID := flags.String("run", "", "agent run ID")
	production := flags.Bool("production", true, "enforce production policy validation")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *policyPath == "" || *receiptsPath == "" ||
		*transactionID == "" || *runID == "" {
		fmt.Fprintln(os.Stderr, "gatemole-model-broker requires --policy, --receipts, --transaction, and --run")
		return 2
	}
	agentToken := os.Getenv("GATEMOLE_MODEL_BROKER_TOKEN")
	providerToken := os.Getenv("GATEMOLE_PROVIDER_BEARER_TOKEN")
	if agentToken == "" || providerToken == "" {
		fmt.Fprintln(os.Stderr, "gatemole-model-broker requires transaction and provider credentials in the environment")
		return 1
	}
	policy, err := modelbroker.LoadPolicy(*policyPath, *production)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	recorder, err := modelbroker.OpenRecorder(
		*receiptsPath, *transactionID, *runID, policy.Provider,
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer recorder.Close()
	logger := log.New(os.Stderr, "gatemole-model-broker: ", log.LstdFlags|log.LUTC)
	broker, err := modelbroker.New(modelbroker.Config{
		Policy: policy, Production: *production,
		TransactionID: *transactionID, RunID: *runID,
		AgentToken: agentToken, ProviderBearerToken: providerToken,
		Recorder: recorder, Logger: logger,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *readyPath != "" {
		if err := os.WriteFile(*readyPath, []byte("ready\n"), 0o600); err != nil {
			_ = listener.Close()
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer os.Remove(*readyPath)
	}
	server := &http.Server{
		Handler:           broker.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       time.Duration(policy.RequestTimeoutSeconds+15) * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	logger.Printf("listening on %s provider=%s transaction=%s", *listen, policy.Provider, *transactionID)
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
