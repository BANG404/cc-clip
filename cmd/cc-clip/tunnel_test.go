package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
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
