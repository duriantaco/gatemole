package gatemole

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	kernelclient "github.com/duriantaco/gatemole/internal/kernel/client"
	"github.com/duriantaco/gatemole/internal/kernel/daemon"
	"github.com/duriantaco/gatemole/internal/kernel/runtimeidentity"
)

const (
	localRuntimeStartupTimeout  = 10 * time.Second
	localRuntimeShutdownTimeout = 6 * time.Second
)

type automaticLocalRuntimeOptions struct {
	command                  string
	socketPath               string
	runtimeEngine            string
	allowUnsafeHostExecution bool
	externallyManagedReason  string
}

// withAutomaticLocalRuntime makes the product-facing local workflow usable
// without asking the operator to keep a second terminal open. It only hosts
// the default development Runtime. An explicitly selected socket, production
// enforcement, or model-broker access remains the responsibility of a
// separately configured daemon.
func withAutomaticLocalRuntime(
	repo string,
	options automaticLocalRuntimeOptions,
	stderr io.Writer,
	operation func() int,
) int {
	if options.socketPath == "" {
		options.socketPath = defaultKernelSocket(repo)
	}
	if options.runtimeEngine == "" {
		options.runtimeEngine = "docker"
	}

	active, err := unixSocketHasListener(options.socketPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: inspect local Runtime socket: %v\n", options.command, err)
		return 1
	}
	if active {
		return operation()
	}
	if options.externallyManagedReason != "" {
		fmt.Fprintf(
			stderr,
			"%s: %s; start `gatemole daemon` with the required configuration before retrying.\n",
			options.command,
			options.externallyManagedReason,
		)
		return operation()
	}

	identity, err := runtimeidentity.Load(context.Background(), repo)
	if err != nil {
		// The command itself owns the established initialization guidance. This
		// path is mostly defensive because callers load the identity first.
		return operation()
	}
	transactionRoot, err := daemon.DefaultTransactionRoot(repo)
	if err != nil {
		fmt.Fprintf(stderr, "%s: select local transaction root: %v\n", options.command, err)
		return 1
	}

	runtimeContext, cancelRuntime := context.WithCancel(context.Background())
	runtimeDone := make(chan error, 1)
	go func() {
		runtimeDone <- daemon.Run(runtimeContext, daemon.Config{
			DatabasePath:    filepath.Join(repo, ".gatemole", "kernel.db"),
			SocketPath:      options.socketPath,
			RepositoryRoot:  repo,
			TransactionRoot: transactionRoot,
			RuntimeProfile:  "development",
			RuntimeEngine: repositoryExecutablePath(
				repo,
				options.runtimeEngine,
			),
			VerifierUID:              os.Getuid(),
			VerifierGID:              os.Getgid(),
			AllowUnsafeHostExecution: options.allowUnsafeHostExecution,
			Stdout:                   io.Discard,
		})
	}()

	started, startErr, runtimeExited := waitForAutomaticLocalRuntime(
		options.socketPath,
		identity.RuntimeID,
		runtimeDone,
	)
	if !started {
		cancelRuntime()
		if !runtimeExited {
			select {
			case <-runtimeDone:
			case <-time.After(localRuntimeShutdownTimeout):
			}
		}
		if startErr == nil {
			startErr = errors.New("startup did not complete")
		}
		fmt.Fprintf(stderr, "%s: start temporary local Runtime: %v\n", options.command, startErr)
		return 1
	}

	fmt.Fprintf(
		stderr,
		"Local Runtime: started temporarily for `%s`; use `gatemole daemon` for a persistent or configured Runtime.\n",
		options.command,
	)
	code := operation()
	cancelRuntime()

	select {
	case runtimeErr := <-runtimeDone:
		if runtimeErr != nil {
			fmt.Fprintf(stderr, "%s: temporary local Runtime stopped with an error: %v\n", options.command, runtimeErr)
			if code == 0 {
				return 1
			}
		}
	case <-time.After(localRuntimeShutdownTimeout):
		fmt.Fprintf(stderr, "%s: temporary local Runtime did not stop cleanly\n", options.command)
		if code == 0 {
			return 1
		}
	}
	return code
}

func waitForAutomaticLocalRuntime(
	socketPath string,
	runtimeID string,
	runtimeDone <-chan error,
) (started bool, err error, runtimeExited bool) {
	deadline := time.NewTimer(localRuntimeStartupTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	client := kernelclient.New(socketPath).WithExpectedRuntimeID(runtimeID)

	for {
		probeContext, cancelProbe := context.WithTimeout(
			context.Background(),
			250*time.Millisecond,
		)
		err := client.Health(probeContext)
		cancelProbe()
		if err == nil {
			return true, nil, false
		}

		select {
		case runtimeErr := <-runtimeDone:
			return false, runtimeErr, true
		case <-deadline.C:
			return false, fmt.Errorf("Runtime was not ready within %s: %w", localRuntimeStartupTimeout, err), false
		case <-ticker.C:
		}
	}
}

func unixSocketHasListener(socketPath string) (bool, error) {
	info, err := os.Lstat(socketPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return false, nil
	}
	connection, err := net.DialTimeout("unix", socketPath, 250*time.Millisecond)
	if err == nil {
		_ = connection.Close()
		return true, nil
	}
	if errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ENOENT) {
		return false, nil
	}
	// Let the daemon's stricter stale-socket recovery produce the authoritative
	// error without sending any credentials to an unverified listener.
	return false, nil
}

func automaticLocalRuntimeForRun(
	repo string,
	args []string,
) automaticLocalRuntimeOptions {
	options := automaticLocalRuntimeOptions{
		command:       "run",
		socketPath:    defaultKernelSocket(repo),
		runtimeEngine: "docker",
	}
	if socket, explicit := commandFlagValue(args, "socket"); explicit {
		options.socketPath = socket
		options.externallyManagedReason = "an explicit daemon socket was selected"
		return options
	}
	if value, ok := commandFlagValue(args, "require-enforcement-profile"); ok && value == "production" {
		options.externallyManagedReason = "production enforcement requires a configured daemon"
		return options
	}
	if value, ok := commandFlagValue(args, "model-provider"); ok && strings.TrimSpace(value) != "" {
		options.externallyManagedReason = "model access requires a configured daemon broker"
		return options
	}
	runtimeClass, _ := commandFlagValue(args, "runtime")
	options.allowUnsafeHostExecution =
		runtimeClass == "host" && hasCommandFlag(args, "unsafe-host")
	return options
}

func automaticLocalRuntimeForAlias(
	repo, alias string,
	args []string,
) automaticLocalRuntimeOptions {
	options := automaticLocalRuntimeOptions{
		command:       alias,
		socketPath:    defaultKernelSocket(repo),
		runtimeEngine: "docker",
	}
	if socket, explicit := commandFlagValue(args, "socket"); explicit {
		options.socketPath = socket
		options.externallyManagedReason = "an explicit daemon socket was selected"
	}
	return options
}

func commandFlagValue(args []string, name string) (string, bool) {
	longName := "--" + name
	shortName := "-" + name
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			return "", false
		}
		if argument == longName || argument == shortName {
			if index+1 >= len(args) {
				return "", true
			}
			return args[index+1], true
		}
		for _, prefix := range []string{longName + "=", shortName + "="} {
			if strings.HasPrefix(argument, prefix) {
				return strings.TrimPrefix(argument, prefix), true
			}
		}
	}
	return "", false
}
