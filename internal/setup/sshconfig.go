package setup

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// SSHConfigChange describes a modification made to ~/.ssh/config.
type SSHConfigChange struct {
	Action string // "created", "added", "ok"
	Detail string
}

// EnsureSSHConfig ensures ~/.ssh/config has required directives for cc-clip:
//   - RemoteForward <port> 127.0.0.1:<port>
//   - ControlMaster no
//   - ControlPath none
//
// If the host block doesn't exist, it is created before "Host *".
// A backup is written to ~/.ssh/config.cc-clip-backup before any modification.
func EnsureSSHConfig(host string, port int) ([]SSHConfigChange, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("cannot determine home directory: %w", err)
	}
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return nil, fmt.Errorf("cannot create ~/.ssh: %w", err)
	}
	return ensureSSHConfigAt(filepath.Join(sshDir, "config"), host, port)
}

// sshHostPattern turns an ssh destination into the pattern a Host block has to
// carry. ssh matches Host patterns against the hostname alone — the user part
// of user@host is consumed while parsing the destination — so a literal
// "Host user@venus" block matches nothing and its RemoteForward never applies.
func sshHostPattern(destination string) string {
	if at := strings.LastIndex(destination, "@"); at >= 0 && at+1 < len(destination) {
		return destination[at+1:]
	}
	return destination
}

func ensureSSHConfigAt(configPath string, destination string, port int) ([]SSHConfigChange, error) {
	host := sshHostPattern(destination)
	content, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("cannot read %s: %w", configPath, err)
	}

	lines := strings.Split(string(content), "\n")
	block := findHostBlock(lines, host)
	rfValue := fmt.Sprintf("%d 127.0.0.1:%d", port, port)
	var changes []SSHConfigChange
	modified := false

	// Where our directives will end up, so we can look at everything OpenSSH
	// reads before them.
	scopeCutoff := len(lines)
	if block != nil {
		scopeCutoff = block.startLine
	} else if star := findHostStarLine(lines); star >= 0 {
		scopeCutoff = star
	}
	changes = append(changes, scopeWarnings(lines, scopeCutoff)...)

	if block == nil {
		newBlock := []string{
			fmt.Sprintf("Host %s", host),
			fmt.Sprintf("    RemoteForward %s", rfValue),
			"    ControlMaster no",
			"    ControlPath none",
			"",
		}
		starLine := findHostStarLine(lines)
		if starLine >= 0 {
			result := make([]string, 0, len(lines)+len(newBlock))
			result = append(result, lines[:starLine]...)
			result = append(result, newBlock...)
			result = append(result, lines[starLine:]...)
			lines = result
		} else {
			if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
				lines = append(lines, "")
			}
			lines = append(lines, newBlock...)
		}
		changes = append(changes, SSHConfigChange{"created", fmt.Sprintf("Host %s (RemoteForward, ControlMaster no, ControlPath none)", host)})
		modified = true
	} else {
		type required struct {
			key   string
			value string
		}
		directives := []required{
			{"RemoteForward", rfValue},
			{"ControlMaster", "no"},
			{"ControlPath", "none"},
		}
		for _, d := range directives {
			key := strings.ToLower(d.key)
			if key == "remoteforward" {
				// Several RemoteForward lines coexist; every one of them
				// applies, so a missing forward is genuinely just an append.
				if block.hasRemoteForward(port) {
					changes = append(changes, SSHConfigChange{"ok", fmt.Sprintf("%s %s", d.key, d.value)})
					continue
				}
				line := fmt.Sprintf("    %s %s", d.key, d.value)
				lines = insertDirectiveInBlock(lines, block, line)
				block.endLine++
				changes = append(changes, SSHConfigChange{"added", fmt.Sprintf("%s %s", d.key, d.value)})
				modified = true
				continue
			}

			// ControlMaster and ControlPath are single-valued and OpenSSH keeps
			// the FIRST value it obtains. Appending "ControlMaster no" below an
			// existing "ControlMaster auto" therefore changed nothing: the
			// pre-existing master was still reused and the RemoteForward still
			// silently failed. Rewrite the first occurrence instead.
			existing, at, found := block.firstDirective(key)
			switch {
			case found && normalizeSSHDirectiveValue(existing) == normalizeSSHDirectiveValue(d.value):
				changes = append(changes, SSHConfigChange{"ok", fmt.Sprintf("%s %s", d.key, d.value)})
			case found:
				lines[at] = fmt.Sprintf("%s%s %s", leadingWhitespace(lines[at]), d.key, d.value)
				changes = append(changes, SSHConfigChange{"updated", fmt.Sprintf("%s %s (was %s)", d.key, d.value, existing)})
				modified = true
			default:
				line := fmt.Sprintf("    %s %s", d.key, d.value)
				lines = insertDirectiveInBlock(lines, block, line)
				block.endLine++
				changes = append(changes, SSHConfigChange{"added", fmt.Sprintf("%s %s", d.key, d.value)})
				modified = true
			}
		}
	}

	if modified {
		// Write the backup BEFORE mutating the live config, and abort on
		// failure. Overwriting ~/.ssh/config without a verified backup would
		// break the safe-to-revert contract: a corrupted write with no backup
		// could leave the user locked out of every host in the file.
		if len(content) > 0 {
			backupPath := configPath + ".cc-clip-backup"
			if err := os.WriteFile(backupPath, content, 0600); err != nil {
				return nil, fmt.Errorf("cannot write backup %s (refusing to modify live config): %w", backupPath, err)
			}
		}
		newContent := strings.Join(lines, "\n")
		if err := os.WriteFile(configPath, []byte(newContent), 0600); err != nil {
			return nil, fmt.Errorf("cannot write %s: %w", configPath, err)
		}
	}

	return changes, nil
}

type sshBlock struct {
	startLine  int
	endLine    int
	directives []sshDirective
}

type sshDirective struct {
	key   string // lowercase
	value string
	line  int // index into the config's line slice
}

// firstDirective returns the value and line index of the first occurrence of
// key in the block — the one OpenSSH actually honours for single-valued
// directives.
func (b *sshBlock) firstDirective(key string) (string, int, bool) {
	for _, d := range b.directives {
		if d.key == key {
			return d.value, d.line, true
		}
	}
	return "", 0, false
}

// leadingWhitespace returns the indentation of a config line so a rewritten
// directive keeps the surrounding file's style.
func leadingWhitespace(line string) string {
	return line[:len(line)-len(strings.TrimLeft(line, " \t"))]
}

func normalizeSSHDirectiveValue(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func (b *sshBlock) hasRemoteForward(port int) bool {
	for _, d := range b.directives {
		if d.key == "remoteforward" && remoteForwardMatches(d.value, port) {
			return true
		}
	}
	return false
}

func remoteForwardMatches(value string, port int) bool {
	fields := strings.Fields(value)
	if len(fields) < 2 || fields[0] != strconv.Itoa(port) {
		return false
	}
	host, hostPort, err := net.SplitHostPort(fields[1])
	if err != nil || hostPort != strconv.Itoa(port) {
		return false
	}
	return isLoopbackForwardHost(host)
}

func isLoopbackForwardHost(host string) bool {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func findHostBlock(lines []string, host string) *sshBlock {
	var block *sshBlock
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if matchesHost(trimmed, host) {
			block = &sshBlock{startLine: i}
			continue
		}
		if block != nil && isBlockStart(trimmed) {
			block.endLine = i
			return block
		}
		if block != nil {
			key, val := parseSSHDirective(trimmed)
			if key != "" {
				block.directives = append(block.directives, sshDirective{
					key:   strings.ToLower(key),
					value: val,
					line:  i,
				})
			}
		}
	}
	if block != nil {
		block.endLine = len(lines)
	}
	return block
}

func matchesHost(trimmed, host string) bool {
	key, value := parseSSHDirective(trimmed)
	if !strings.EqualFold(key, "Host") {
		return false
	}
	for _, f := range strings.Fields(value) {
		if f == host {
			return true
		}
	}
	return false
}

// isBlockStart reports whether a line opens a new configuration scope.
//
// BOTH Host and Match do. Treating only Host as a boundary let a Host block
// run on through a following Match block, so cc-clip rewrote ControlMaster and
// ControlPath belonging to OTHER connections and appended its RemoteForward
// into that foreign scope — while the targeted Host block received nothing and
// all three edits were reported as successful.
func isBlockStart(trimmed string) bool {
	key, _ := parseSSHDirective(trimmed)
	return strings.EqualFold(key, "Host") || strings.EqualFold(key, "Match")
}

func findHostStarLine(lines []string) int {
	for i, line := range lines {
		if matchesHost(strings.TrimSpace(line), "*") {
			return i
		}
	}
	return -1
}

// parseSSHDirective splits one config line into keyword and arguments.
//
// ssh_config separates the two by whitespace or by exactly one "=", so a
// TAB-separated directive is as valid as a space-separated one. Splitting on
// the literal " " alone made "ControlMaster\tauto" parse as a single opaque
// keyword, so the conflict check never saw it and appended a second
// ControlMaster that OpenSSH then ignored.
func parseSSHDirective(line string) (string, string) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", ""
	}
	i := strings.IndexAny(trimmed, " \t=")
	if i < 0 {
		return trimmed, ""
	}
	key := trimmed[:i]
	value := strings.TrimSpace(strings.TrimLeft(trimmed[i:], " \t="))
	return key, value
}

func insertDirectiveInBlock(lines []string, block *sshBlock, directive string) []string {
	insertAt := block.endLine
	for insertAt > block.startLine+1 && strings.TrimSpace(lines[insertAt-1]) == "" {
		insertAt--
	}
	result := make([]string, 0, len(lines)+1)
	result = append(result, lines[:insertAt]...)
	result = append(result, directive)
	result = append(result, lines[insertAt:]...)
	return result
}

// scopeWarnings reports anything ABOVE our directives that can win the
// first-obtained-value race, so cc-clip never claims to have fixed an
// effective configuration it cannot actually see.
//
// Two cases matter and neither is resolvable from this file alone: an Include
// pulls in directives from files this code does not read, and a Host/Match
// block that OpenSSH reaches first can already have set ControlMaster or
// ControlPath. Reporting them as warnings — rather than editing another
// scope, or staying silent — keeps the safe-to-revert contract while telling
// the user exactly what to verify.
func scopeWarnings(lines []string, cutoff int) []SSHConfigChange {
	var out []SSHConfigChange
	sawInclude := false
	sawConflict := false

	for i := 0; i < cutoff && i < len(lines); i++ {
		key, _ := parseSSHDirective(strings.TrimSpace(lines[i]))
		switch {
		case strings.EqualFold(key, "Include"):
			sawInclude = true
		case strings.EqualFold(key, "ControlMaster"), strings.EqualFold(key, "ControlPath"):
			sawConflict = true
		}
	}

	if sawInclude {
		out = append(out, SSHConfigChange{"warning",
			"an Include appears before this host; cc-clip cannot see what it sets. Verify with: ssh -G <host> | grep -i control"})
	}
	if sawConflict {
		out = append(out, SSHConfigChange{"warning",
			"an earlier Host/Match block already sets ControlMaster or ControlPath and OpenSSH keeps the first value it obtains. Verify with: ssh -G <host> | grep -i control"})
	}
	return out
}
