package tunnel

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const IdentityProtocolVersion = 1

var (
	ErrIdentityEndpointUnavailable = errors.New("tunnel identity endpoint unavailable")
	ErrIdentityUnauthorized        = errors.New("tunnel identity token rejected")
)

// IdentityInfo is returned by the token-protected /tunnel/identity endpoint.
type IdentityInfo struct {
	Service         string `json:"service"`
	Status          string `json:"status"`
	ProtocolVersion int    `json:"protocol_version"`
	InstanceID      string `json:"instance_id"`
}

// FetchIdentity authenticates to a local daemon and validates its managed-
// tunnel identity contract.
func FetchIdentity(addr, bearerToken string, timeout time.Duration) (IdentityInfo, error) {
	ctxClient := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/tunnel/identity", nil)
	if err != nil {
		return IdentityInfo{}, err
	}
	req.Header.Set("Authorization", "Bearer "+bearerToken)
	req.Header.Set("User-Agent", "cc-clip")
	resp, err := ctxClient.Do(req)
	if err != nil {
		return IdentityInfo{}, fmt.Errorf("GET /tunnel/identity: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return IdentityInfo{}, ErrIdentityUnauthorized
	case http.StatusNotFound, http.StatusServiceUnavailable:
		return IdentityInfo{}, ErrIdentityEndpointUnavailable
	case http.StatusOK:
	default:
		return IdentityInfo{}, fmt.Errorf("GET /tunnel/identity -> %d", resp.StatusCode)
	}
	var identity IdentityInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&identity); err != nil {
		return IdentityInfo{}, fmt.Errorf("invalid /tunnel/identity body: %w", err)
	}
	if err := identity.Validate(); err != nil {
		return IdentityInfo{}, err
	}
	return identity, nil
}

// Validate checks the identity protocol before a caller trusts InstanceID.
func (i IdentityInfo) Validate() error {
	if i.Service != "cc-clip" || i.Status != "ok" {
		return fmt.Errorf("invalid tunnel identity service/status")
	}
	if i.ProtocolVersion != IdentityProtocolVersion {
		return fmt.Errorf("unsupported tunnel identity protocol %d", i.ProtocolVersion)
	}
	if i.InstanceID == "" {
		return fmt.Errorf("tunnel identity has empty instance_id")
	}
	return nil
}

type RemoteIdentityState string

const (
	RemoteIdentityOK                  RemoteIdentityState = "ok"
	RemoteIdentityHelperMissing       RemoteIdentityState = "helper-missing"
	RemoteIdentityEndpointUnavailable RemoteIdentityState = "endpoint-unavailable"
	RemoteIdentityTokenInvalid        RemoteIdentityState = "token-invalid"
	RemoteIdentityUnknown             RemoteIdentityState = "unknown"
)

const (
	identityMarkerOK                  = "cc-clip-identity:ok:"
	identityMarkerHelperMissing       = "cc-clip-identity:helper-missing"
	identityMarkerEndpointUnavailable = "cc-clip-identity:endpoint-unavailable"
	identityMarkerTokenInvalid        = "cc-clip-identity:token-invalid"
	identityMarkerUnknown             = "cc-clip-identity:unknown"
)

// RemoteIdentityProbeCommand builds a fixed remote shell command that invokes
// the deployed helper. The helper reads the synchronized token itself, keeping
// the credential out of the ssh argument list and the generated command.
func RemoteIdentityProbeCommand(port int) string {
	return fmt.Sprintf(`_cc_helper="$HOME/.local/bin/cc-clip"
if [ ! -x "$_cc_helper" ]; then
  echo '%[2]s'
elif ! "$_cc_helper" tunnel probe-identity --port %[1]d 2>/dev/null; then
  echo '%[2]s'
fi`, port, identityMarkerHelperMissing)
}

// RemoteIdentityProbeOutput encodes a helper result using the marker protocol
// consumed by ClassifyRemoteIdentityProbeOutput. Invalid successful payloads
// are downgraded to unknown.
func RemoteIdentityProbeOutput(state RemoteIdentityState, identity IdentityInfo) string {
	switch state {
	case RemoteIdentityOK:
		if err := identity.Validate(); err != nil {
			return identityMarkerUnknown
		}
		data, err := json.Marshal(identity)
		if err != nil {
			return identityMarkerUnknown
		}
		return identityMarkerOK + string(data)
	case RemoteIdentityHelperMissing:
		return identityMarkerHelperMissing
	case RemoteIdentityEndpointUnavailable:
		return identityMarkerEndpointUnavailable
	case RemoteIdentityTokenInvalid:
		return identityMarkerTokenInvalid
	default:
		return identityMarkerUnknown
	}
}

// ClassifyRemoteIdentityProbeOutput preserves capability and authentication
// failures as distinct states and validates successful identity JSON.
func ClassifyRemoteIdentityProbeOutput(out string) (RemoteIdentityState, IdentityInfo) {
	if idx := strings.Index(out, identityMarkerOK); idx >= 0 {
		dec := json.NewDecoder(strings.NewReader(out[idx+len(identityMarkerOK):]))
		var identity IdentityInfo
		if err := dec.Decode(&identity); err == nil && identity.Validate() == nil {
			return RemoteIdentityOK, identity
		}
		return RemoteIdentityUnknown, IdentityInfo{}
	}
	switch {
	case strings.Contains(out, identityMarkerTokenInvalid):
		return RemoteIdentityTokenInvalid, IdentityInfo{}
	case strings.Contains(out, identityMarkerHelperMissing):
		return RemoteIdentityHelperMissing, IdentityInfo{}
	case strings.Contains(out, identityMarkerEndpointUnavailable):
		return RemoteIdentityEndpointUnavailable, IdentityInfo{}
	case strings.Contains(out, identityMarkerUnknown):
		return RemoteIdentityUnknown, IdentityInfo{}
	default:
		return RemoteIdentityUnknown, IdentityInfo{}
	}
}
