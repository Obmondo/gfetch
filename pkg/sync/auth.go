package sync

import (
	"fmt"

	"github.com/ashish1099/gfetch/pkg/config"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
)

// resolveAuth returns the appropriate auth method for a repo.
// HTTPS public repos use anonymous (nil) auth; SSH repos use key-based auth
// with built-in host key verification (plus any extra entries from ssh_known_hosts).
func resolveAuth(repo *config.RepoConfig) (transport.AuthMethod, error) {
	if repo.IsHTTPS() {
		return nil, nil
	}
	return sshAuth(repo.SSHKeyPath, repo.SSHKnownHosts)
}

// sshAuth creates an SSH public key auth method from a key file.
// Host key verification always uses built-in known_hosts for major providers,
// merged with any extra user-provided entries.
func sshAuth(keyPath, extraKnownHosts string) (*gitssh.PublicKeys, error) {
	auth, err := gitssh.NewPublicKeysFromFile("git", keyPath, "")
	if err != nil {
		return nil, fmt.Errorf("loading SSH key %s: %w", keyPath, err)
	}

	hostKeyCallback, err := buildKnownHostsCallback(extraKnownHosts)
	if err != nil {
		return nil, fmt.Errorf("building known_hosts callback: %w", err)
	}
	auth.HostKeyCallback = hostKeyCallback

	return auth, nil
}
