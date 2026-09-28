package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shunmei/cc-clip/internal/token"
	"github.com/shunmei/cc-clip/internal/tunnel"
)

func TestCmdTunnelProbeIdentity(t *testing.T) {
	token.TokenDirOverride = t.TempDir()
	defer func() { token.TokenDirOverride = "" }()
	if _, err := token.WriteTokenFile("token-123", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token-123" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"service":"cc-clip","status":"ok","protocol_version":1,"instance_id":"instance-123"}`)
	}))
	defer ts.Close()
	_, portText, err := net.SplitHostPort(ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	originalArgs := os.Args
	os.Args = []string{"cc-clip", "tunnel", "probe-identity", "--port", strconv.Itoa(port)}
	defer func() { os.Args = originalArgs }()

	var out bytes.Buffer
	cmdTunnelProbeIdentity(&out)
	state, identity := tunnel.ClassifyRemoteIdentityProbeOutput(out.String())
	if state != tunnel.RemoteIdentityOK || identity.InstanceID != "instance-123" {
		t.Fatalf("helper result = %q %+v; output=%s", state, identity, out.String())
	}
}

func TestCmdTunnelProbeIdentityReportsEndpointUnavailable(t *testing.T) {
	for name, status := range map[string]int{
		"missing endpoint":     http.StatusNotFound,
		"identity unavailable": http.StatusServiceUnavailable,
	} {
		t.Run(name, func(t *testing.T) {
			token.TokenDirOverride = t.TempDir()
			t.Cleanup(func() { token.TokenDirOverride = "" })
			if _, err := token.WriteTokenFile("token-123", time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}

			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, http.StatusText(status), status)
			}))
			t.Cleanup(ts.Close)
			_, portText, err := net.SplitHostPort(ts.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			port, err := strconv.Atoi(portText)
			if err != nil {
				t.Fatal(err)
			}

			originalArgs := os.Args
			os.Args = []string{"cc-clip", "tunnel", "probe-identity", "--port", strconv.Itoa(port)}
			t.Cleanup(func() { os.Args = originalArgs })
			var out bytes.Buffer
			cmdTunnelProbeIdentity(&out)
			if state, _ := tunnel.ClassifyRemoteIdentityProbeOutput(out.String()); state != tunnel.RemoteIdentityEndpointUnavailable {
				t.Fatalf("helper state = %q, want endpoint-unavailable; output=%s", state, out.String())
			}
		})
	}
}

func TestParseTunnelRunArgsAllowsEqualsPort(t *testing.T) {
	originalArgs := os.Args
	os.Args = []string{"cc-clip", "tunnel", "run", "example-host", "--port=18340"}
	t.Cleanup(func() { os.Args = originalArgs })

	host, reset, err := parseTunnelRunArgs(os.Args[3:])
	if err != nil {
		t.Fatalf("parseTunnelRunArgs() error = %v", err)
	}
	if host != "example-host" {
		t.Fatalf("parseTunnelRunArgs() host = %q, want example-host", host)
	}
	if reset {
		t.Fatal("parseTunnelRunArgs() reset = true, want false")
	}
	if port := getPort(); port != 18340 {
		t.Fatalf("getPort() = %d, want 18340", port)
	}
}

func TestTunnelUsageDocumentsCurrentLimitations(t *testing.T) {
	var out bytes.Buffer
	tunnelUsage(&out)
	usage := strings.Join(strings.Fields(out.String()), " ")
	for _, want := range []string{
		`cc-clip connect <host> --force`,
		`$HOME/.local/bin/cc-clip`,
		`--use-remote-bin`,
		`30-day sliding expiration`,
		`token active while the supervisor runs`,
	} {
		if !strings.Contains(usage, want) {
			t.Errorf("tunnelUsage() missing %q", want)
		}
	}
}
