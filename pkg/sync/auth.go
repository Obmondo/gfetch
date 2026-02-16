package sync

import (
	"fmt"

	"github.com/ashish1099/gitsync/pkg/config"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
)

// resolveAuth returns the appropriate auth method for a repo.
// HTTPS public repos use anonymous (nil) auth; SSH repos use key-based auth.
func resolveAuth(repo *config.RepoConfig) (transport.AuthMethod, error) {
	if repo.IsHTTPS() {
		return nil, nil
	}
	return sshAuth(repo.SSHKeyPath)
}

// sshAuth creates an SSH public key auth method from a key file.
func sshAuth(keyPath string) (*gitssh.PublicKeys, error) {
	auth, err := gitssh.NewPublicKeysFromFile("git", keyPath, "")
	if err != nil {
		return nil, fmt.Errorf("loading SSH key %s: %w", keyPath, err)
	}
	return auth, nil
}
