package scripts_test

import (
	"os"
	"strings"
	"testing"
)

func TestWindowsInstallScriptVerifiesReleaseChecksumBeforeExtraction(t *testing.T) {
	data, err := os.ReadFile("install.ps1")
	if err != nil {
		t.Fatalf("read install.ps1: %v", err)
	}
	script := string(data)

	for _, needle := range []string{
		"checksums.txt",
		"Verify-Checksum",
		"Get-FileHash -Algorithm SHA256",
		"Expand-Archive",
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("install.ps1 must contain %q", needle)
		}
	}

	verifyIdx := strings.Index(script, "Verify-Checksum $archivePath")
	extractIdx := strings.Index(script, "Expand-Archive")
	if verifyIdx == -1 || extractIdx == -1 || verifyIdx > extractIdx {
		t.Fatalf("install.ps1 must verify the archive checksum before extraction")
	}
}

func TestWindowsInstallScriptSupportsCCClipVersionPin(t *testing.T) {
	data, err := os.ReadFile("install.ps1")
	if err != nil {
		t.Fatalf("read install.ps1: %v", err)
	}
	script := string(data)

	for _, needle := range []string{
		"CC_CLIP_VERSION",
		"Resolve-Version",
		"^v[0-9]+\\.[0-9]+\\.[0-9]+",
		"pinned via CC_CLIP_VERSION",
		"is not a valid version tag",
		"releases/latest",
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("install.ps1 must contain %q for CC_CLIP_VERSION pinning", needle)
		}
	}

	resolveIdx := strings.Index(script, "$version = Resolve-Version")
	archiveIdx := strings.Index(script, "$archiveName =")
	if resolveIdx == -1 || archiveIdx == -1 || resolveIdx > archiveIdx {
		t.Fatalf("install.ps1 must resolve the version before building archiveName")
	}
}

func TestWindowsInstallScriptPinsTLS12BeforeNetworkCalls(t *testing.T) {
	data, err := os.ReadFile("install.ps1")
	if err != nil {
		t.Fatalf("read install.ps1: %v", err)
	}
	script := string(data)

	tlsIdx := strings.Index(script, "[Net.ServicePointManager]::SecurityProtocol")
	if tlsIdx == -1 {
		t.Fatal("install.ps1 must pin TLS 1.2 or newer before network calls")
	}
	for _, needle := range []string{"Invoke-RestMethod", "Invoke-WebRequest"} {
		callIdx := strings.Index(script, needle)
		if callIdx == -1 {
			t.Fatalf("install.ps1 must contain %q", needle)
		}
		if tlsIdx > callIdx {
			t.Fatalf("install.ps1 must pin TLS before %s", needle)
		}
	}
}

func TestWindowsInstallScriptUsesWindowsZipReleaseContract(t *testing.T) {
	data, err := os.ReadFile("install.ps1")
	if err != nil {
		t.Fatalf("read install.ps1: %v", err)
	}
	script := string(data)

	for _, needle := range []string{
		`$platform = "windows_$arch"`,
		`$archiveName = "cc-clip_$($version.TrimStart("v"))_${platform}.zip"`,
		"cc-clip.exe",
		"x64",
		"amd64",
		"arm64",
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("install.ps1 must contain %q for the Windows zip release contract", needle)
		}
	}
}

// TestWindowsInstallScriptFallsBackWhenTheAPIIsRateLimited pins the #170
// fallback for Windows (review on #172): the latest-tag lookup must not
// depend on the GitHub API alone (60 requests/hour per public IP, shared
// behind a NAT), and a failed lookup must say how to pin a version.
func TestWindowsInstallScriptFallsBackWhenTheAPIIsRateLimited(t *testing.T) {
	data, err := os.ReadFile("install.ps1")
	if err != nil {
		t.Fatalf("read install.ps1: %v", err)
	}
	script := string(data)

	for _, needle := range []string{
		// API first, then the non-rate-limited releases/latest redirect,
		// read without following it.
		"https://api.github.com/repos/$Repo/releases/latest",
		"https://github.com/$Repo/releases/latest",
		"AllowAutoRedirect = $false",
		"/releases/tag/",
		// A failed lookup explains the rate limit and how to pin a version.
		"60 API requests/hour",
		`$env:CC_CLIP_VERSION = "vX.Y.Z"`,
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("install.ps1 must contain %q for the rate-limit fallback", needle)
		}
	}

	getLatest := strings.Index(script, "function Get-LatestVersion {")
	if getLatest == -1 {
		t.Fatal("install.ps1 must define Get-LatestVersion")
	}
	body := script[getLatest:]
	apiIdx := strings.Index(body, "api.github.com")
	redirectIdx := strings.Index(body, "Get-LatestVersionFromRedirect")
	if apiIdx == -1 || redirectIdx == -1 || apiIdx > redirectIdx {
		t.Fatal("Get-LatestVersion must try the API before the releases/latest redirect")
	}
}
