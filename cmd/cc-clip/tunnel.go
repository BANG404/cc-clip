package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/shunmei/cc-clip/internal/instanceid"
	"github.com/shunmei/cc-clip/internal/token"
	"github.com/shunmei/cc-clip/internal/tunnel"
	"github.com/shunmei/cc-clip/internal/tunnelmgr"
)

// cmdTunnel implements `cc-clip tunnel <subcommand>`.
//
// Phase 1A of the managed persistent tunnel (issue #108) ships exactly one
// user-facing subcommand: `run`, a foreground, manually started supervisor
// for a single host. It exists so the lifecycle logic can be exercised on real hosts
// before any service-manager work is layered on. It does not install a
// LaunchAgent, does not touch ~/.ssh/config, and does not change the default
// behavior of any other command — the interactive RemoteForward workflow
// remains the only automatic tunnel path.
func cmdTunnel() {
	if len(os.Args) < 3 {
		tunnelUsage(os.Stderr)
		os.Exit(2)
	}
	switch os.Args[2] {
	case "run":
		cmdTunnelRun(os.Args[3:])
	case "probe-identity":
		cmdTunnelProbeIdentity(os.Stdout)
	case "-h", "--help", "help":
		tunnelUsage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: tunnel %s\n", os.Args[2])
		tunnelUsage(os.Stderr)
		os.Exit(2)
	}
}

// cmdTunnelProbeIdentity is the remote capability used by the supervisor.
// It is intentionally omitted from user-facing help: callers deploy it via
// the existing connect flow and invoke it over the private SSH master.
func cmdTunnelProbeIdentity(w io.Writer) {
	state := tunnel.RemoteIdentityUnknown
	var identity tunnel.IdentityInfo
	tok, err := token.ReadTokenFile()
	if err != nil {
		state = tunnel.RemoteIdentityTokenInvalid
	} else {
		identity, err = tunnel.FetchIdentity(fmt.Sprintf("127.0.0.1:%d", getPort()), tok, 5*time.Second)
		switch {
		case err == nil:
			state = tunnel.RemoteIdentityOK
		case errors.Is(err, tunnel.ErrIdentityUnauthorized):
			state = tunnel.RemoteIdentityTokenInvalid
		case errors.Is(err, tunnel.ErrIdentityEndpointUnavailable):
			state = tunnel.RemoteIdentityEndpointUnavailable
		}
	}
	fmt.Fprintln(w, tunnel.RemoteIdentityProbeOutput(state, identity))
}

func tunnelUsage(w io.Writer) {
	fmt.Fprintln(w, `usage: cc-clip tunnel run <host> [--port N] [--reset]

Subcommands:
  run <host>         Run the managed tunnel supervisor for <host> in the
                     foreground (experimental, manual only).

The supervisor starts a private non-interactive ssh master
(BatchMode, ClearAllForwardings, ExitOnForwardFailure) that holds exactly
one reverse forward 127.0.0.1:<port> -> 127.0.0.1:<port>, probes the local
daemon's health through the forward from the remote side, and reconnects
with bounded backoff. It stops cleanly on Ctrl-C / SIGTERM.

It does NOT install a LaunchAgent or write ~/.ssh/config. ssh reads the named
Host normally so HostName, User, IdentityFile and ProxyJump keep working.
It does NOT enable anything automatically. Your existing interactive
RemoteForward setup keeps working exactly as before; stop this command
before relying on the managed forward (both bind the same remote port).

State is persisted per host under ~/.cache/cc-clip/tunnels/ (mode 0600).
Use --reset to explicitly rebuild corrupt runtime or clear crash-loop state.`)
}

func cmdTunnelRun(args []string) {
	host := ""
	reset := false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--port":
			i++ // value consumed by getPort()
		case args[i] == "--reset":
			reset = true
		case strings.HasPrefix(args[i], "-"):
			fmt.Fprintf(os.Stderr, "cc-clip tunnel run: unknown flag %s\n", args[i])
			os.Exit(2)
		case host == "":
			host = args[i]
		default:
			fmt.Fprintln(os.Stderr, "cc-clip tunnel run: exactly one <host> argument is required")
			os.Exit(2)
		}
	}
	if host == "" {
		tunnelUsage(os.Stderr)
		os.Exit(2)
	}

	tokenDir, err := token.TokenDir()
	if err != nil {
		log.Fatalf("cc-clip tunnel run: resolve state directory: %v", err)
	}
	expectedInstanceID, err := instanceid.LoadOrCreate(tokenDir)
	if err != nil {
		log.Fatalf("cc-clip tunnel run: load instance identity: %v", err)
	}
	spec := tunnelmgr.Spec{Host: host, Port: getPort(), ExpectedInstanceID: expectedInstanceID}
	statePath, err := tunnelmgr.DefaultStorePath(host)
	if err != nil {
		log.Fatalf("cc-clip tunnel run: resolve state path: %v", err)
	}
	store := tunnelmgr.NewStoreAt(statePath)
	if reset {
		if err := store.Reset(spec); err != nil {
			log.Fatalf("cc-clip tunnel run: reset state: %v", err)
		}
	}
	backend := &tunnelmgr.Backend{Spec: spec}
	sup := tunnelmgr.NewSupervisor(spec, store, backend)
	sup.Logf = func(format string, args ...any) {
		log.Printf(format, args...)
	}

	// shutdownSignals() is SIGINT+SIGTERM on Unix, Ctrl+C on Windows.
	ctx, stop := signal.NotifyContext(context.Background(), shutdownSignals()...)
	defer stop()

	log.Printf("managed tunnel starting for %s on port %d (state: %s)", host, spec.Port, statePath)
	if err := sup.Run(ctx); err != nil {
		if errors.Is(err, tunnelmgr.ErrCrashLoopOpen) {
			log.Printf("cc-clip tunnel run: %v", err)
			return
		}
		// A state file this build cannot validate is fail-closed: refuse to
		// run rather than guess. The file is named so the operator can
		// inspect it; nothing was started.
		log.Fatalf("cc-clip tunnel run: %v", err)
	}
	log.Printf("managed tunnel stopped")
}
