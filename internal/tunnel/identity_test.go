package tunnel

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoteIdentityProbeCommandKeepsTokenOutOfArgv(t *testing.T) {
	cmd := RemoteIdentityProbeCommand(18339)
	for _, want := range []string{"$HOME/.local/bin/cc-clip", "tunnel probe-identity --port 18339"} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("remote identity command missing %q", want)
		}
	}
	for _, unwanted := range []string{"session.token", "Authorization:", "curl"} {
		if strings.Contains(cmd, unwanted) {
			t.Fatalf("remote identity command must leave %q to the deployed helper", unwanted)
		}
	}
}

func TestRemoteIdentityProbeUsesDeployedHelper(t *testing.T) {
	requireProbeShellTools(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	helperDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(helperDir, 0o700); err != nil {
		t.Fatal(err)
	}
	want := `cc-clip-identity:ok:{"service":"cc-clip","status":"ok","protocol_version":1,"instance_id":"instance-123"}`
	helper := "#!/bin/sh\nprintf '%s\\n' '" + want + "'\n"
	if err := os.WriteFile(filepath.Join(helperDir, "cc-clip"), []byte(helper), 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", "-c", RemoteIdentityProbeCommand(18339)).CombinedOutput()
	if err != nil {
		t.Fatalf("identity command: %v: %s", err, out)
	}
	state, identity := ClassifyRemoteIdentityProbeOutput(string(out))
	if state != RemoteIdentityOK || identity.InstanceID != "instance-123" {
		t.Fatalf("identity result = %q %+v; output=%s", state, identity, out)
	}
}

func TestRemoteIdentityProbeMissingHelperIsDistinct(t *testing.T) {
	requireProbeShellTools(t)
	t.Setenv("HOME", t.TempDir())
	out, err := exec.Command("sh", "-c", RemoteIdentityProbeCommand(18339)).CombinedOutput()
	if err != nil {
		t.Fatalf("identity command: %v: %s", err, out)
	}
	if state, _ := ClassifyRemoteIdentityProbeOutput(string(out)); state != RemoteIdentityHelperMissing {
		t.Fatalf("missing helper state = %q, output=%s", state, out)
	}
}

func TestRemoteIdentityProbeUnsupportedHelperIsMissingCapability(t *testing.T) {
	requireProbeShellTools(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	helperDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(helperDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(helperDir, "cc-clip"), []byte("#!/bin/sh\nexit 2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", "-c", RemoteIdentityProbeCommand(18339)).CombinedOutput()
	if err != nil {
		t.Fatalf("identity command: %v: %s", err, out)
	}
	if state, _ := ClassifyRemoteIdentityProbeOutput(string(out)); state != RemoteIdentityHelperMissing {
		t.Fatalf("unsupported helper state = %q, output=%s", state, out)
	}
}

func TestRemoteIdentityProbeOutput(t *testing.T) {
	identity := IdentityInfo{Service: "cc-clip", Status: "ok", ProtocolVersion: 1, InstanceID: "abc"}
	out := RemoteIdentityProbeOutput(RemoteIdentityOK, identity)
	if state, got := ClassifyRemoteIdentityProbeOutput(out); state != RemoteIdentityOK || got != identity {
		t.Fatalf("round trip = %q %+v; output=%s", state, got, out)
	}
	if got := RemoteIdentityProbeOutput(RemoteIdentityOK, IdentityInfo{}); got != identityMarkerUnknown {
		t.Fatalf("invalid successful identity = %q, want unknown marker", got)
	}
	for state, marker := range map[RemoteIdentityState]string{
		RemoteIdentityHelperMissing:       identityMarkerHelperMissing,
		RemoteIdentityEndpointUnavailable: identityMarkerEndpointUnavailable,
		RemoteIdentityTokenInvalid:        identityMarkerTokenInvalid,
	} {
		if got := RemoteIdentityProbeOutput(state, IdentityInfo{}); got != marker {
			t.Fatalf("output for %q = %q, want %q", state, got, marker)
		}
	}
}

func TestClassifyRemoteIdentityProbeOutput(t *testing.T) {
	valid := `cc-clip-identity:ok:{"service":"cc-clip","status":"ok","protocol_version":1,"instance_id":"abc"}`
	state, identity := ClassifyRemoteIdentityProbeOutput(valid)
	if state != RemoteIdentityOK || identity.InstanceID != "abc" {
		t.Fatalf("valid identity = %q %+v", state, identity)
	}
	for marker, want := range map[string]RemoteIdentityState{
		identityMarkerHelperMissing:       RemoteIdentityHelperMissing,
		identityMarkerEndpointUnavailable: RemoteIdentityEndpointUnavailable,
		identityMarkerTokenInvalid:        RemoteIdentityTokenInvalid,
		identityMarkerUnknown:             RemoteIdentityUnknown,
		"motd only":                       RemoteIdentityUnknown,
	} {
		if got, _ := ClassifyRemoteIdentityProbeOutput(marker); got != want {
			t.Fatalf("classify %q = %q, want %q", marker, got, want)
		}
	}
}
