package theory

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// MarkerRef is the shared marker, the tag theory/compiled (LOCK-7).
const MarkerRef = "refs/tags/theory/compiled"

const zeroHash = "0000000000000000000000000000000000000000"

var vaultName = regexp.MustCompile(`^[a-z0-9-]+$`)
var fullHash = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ValidVaultName reports whether name uses only lowercase letters, digits and hyphens (GIT-1).
func ValidVaultName(name string) bool { return vaultName.MatchString(name) }

// VaultsDir is where the vaults live: /data/vaults in the Docker image (GIT-2).
func VaultsDir(dataDir string) string { return filepath.Join(dataDir, "vaults") }

// VaultPath is the bare repository of a vault (GIT-2).
func VaultPath(dataDir, name string) string { return filepath.Join(VaultsDir(dataDir), name+".git") }

// VaultExists reports whether the vault exists.
func VaultExists(dataDir, name string) bool {
	if !ValidVaultName(name) {
		return false
	}
	info, err := os.Stat(VaultPath(dataDir, name))
	return err == nil && info.IsDir()
}

// git runs git in dir and returns its trimmed output.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errOut.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// CreateVault makes an empty bare repository for a vault (GIT-1).
func CreateVault(dataDir, name string) error {
	if !ValidVaultName(name) {
		return fmt.Errorf("%q isn't a valid vault name. Use only lowercase letters, digits and hyphens.", name)
	}
	path := VaultPath(dataDir, name)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("The vault %s already exists.", name)
	}
	if err := os.MkdirAll(VaultsDir(dataDir), 0o755); err != nil {
		return err
	}
	if _, err := git(VaultsDir(dataDir), "init", "--bare", "--quiet", "--initial-branch=main", path); err != nil {
		return err
	}
	return InstallHook(path)
}

// hookScript runs the server's own program as the pre-receive hook (GIT-9).
// The server passes its own path and the pusher in the environment; a push
// that doesn't come through the server is rejected.
const hookScript = `#!/bin/sh
# Installed by theory-server. The push rules live in "theory-server hook pre-receive".
if [ -z "$THEORY_SERVER_BIN" ]; then
	echo "Rejected: push through theory-server." >&2
	exit 1
fi
exec "$THEORY_SERVER_BIN" hook pre-receive
`

// InstallHook writes the pre-receive hook into a vault.
func InstallHook(vaultPath string) error {
	hooks := filepath.Join(vaultPath, "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(hooks, "pre-receive"), []byte(hookScript), 0o755)
}

// InstallHooks writes the hook into every vault, so they all run this version's rules.
func InstallHooks(dataDir string) error {
	entries, err := os.ReadDir(VaultsDir(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".git")
		if e.IsDir() && ok && ValidVaultName(name) {
			if err := InstallHook(filepath.Join(VaultsDir(dataDir), e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// Marker returns the commit the marker points at, or "" when there is none.
func Marker(vaultPath string) string {
	out, err := git(vaultPath, "rev-parse", "--verify", "--quiet", MarkerRef+"^{commit}")
	if err != nil {
		return ""
	}
	return out
}

// MarkerError is a marker the release can't move to (LOCK-9).
type MarkerError struct{ Message string }

func (e *MarkerError) Error() string { return e.Message }

// CheckMarker checks that commit is on main and comes after the current marker (LOCK-9).
func CheckMarker(vaultPath, commit string) error {
	if !fullHash.MatchString(commit) {
		return &MarkerError{fmt.Sprintf("The marker must be a full commit hash, not %q.", commit)}
	}
	if _, err := git(vaultPath, "merge-base", "--is-ancestor", commit, "refs/heads/main"); err != nil {
		return &MarkerError{fmt.Sprintf("Commit %s isn't on main of this vault, so the marker can't move to it. Sync, then try again.", commit[:7])}
	}
	current := Marker(vaultPath)
	if current == "" {
		return nil
	}
	if current == commit {
		return &MarkerError{fmt.Sprintf("The marker is already at %s. It only moves forward.", commit[:7])}
	}
	if _, err := git(vaultPath, "merge-base", "--is-ancestor", current, commit); err != nil {
		return &MarkerError{fmt.Sprintf("Commit %s doesn't come after the marker at %s. The marker only moves forward.", commit[:7], current[:7])}
	}
	return nil
}

// MoveMarker moves the marker to commit after checking it (LOCK-9). The
// returned undo puts the marker back.
func MoveMarker(vaultPath, commit string) (undo func(), err error) {
	if err := CheckMarker(vaultPath, commit); err != nil {
		return nil, err
	}
	current := Marker(vaultPath)
	old := current
	if old == "" {
		old = zeroHash
	}
	if _, err := git(vaultPath, "update-ref", "-m", "theory-server: compile released", MarkerRef, commit, old); err != nil {
		return nil, err
	}
	return func() {
		if current == "" {
			git(vaultPath, "update-ref", "-d", MarkerRef, commit)
		} else {
			git(vaultPath, "update-ref", MarkerRef, current, commit)
		}
	}, nil
}
