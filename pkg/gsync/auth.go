package gsync

import (
	"flag"
	"fmt"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/transport"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh"

	"github.com/obmondo/gfetch/pkg/config"
)

// defaultSSHPort is used when transport.NewEndpoint reports Port == 0 (i.e.
// the URL didn't specify one).
const defaultSSHPort = 22

// preferredHostKeyAlgorithms mirrors OpenSSH 9.x's default HostKeyAlgorithms
// order (`man ssh_config` → HostKeyAlgorithms): certificate variants first,
// then plain keys; within each group Ed25519, then ECDSA (256/384/521),
// then security-key variants, then RSA-SHA2. The deprecated ssh-rsa (SHA-1)
// and ssh-dss are intentionally omitted, also matching OpenSSH.
//
// This list is used as the *fallback* preference order. At connection time
// the algorithms our known_hosts actually has entries for at this host:port
// are promoted to the front via mergeAlgorithms, mimicking OpenSSH's dynamic
// HostKeyAlgorithms promotion: "prefer what we already know for this host,
// fall back to the standard preference for everything else."
var preferredHostKeyAlgorithms = []string{
	// Certificate variants.
	ssh.CertAlgoED25519v01,
	ssh.CertAlgoECDSA256v01,
	ssh.CertAlgoECDSA384v01,
	ssh.CertAlgoECDSA521v01,
	ssh.CertAlgoSKED25519v01,
	ssh.CertAlgoSKECDSA256v01,
	ssh.CertAlgoRSASHA512v01,
	ssh.CertAlgoRSASHA256v01,

	// Plain keys.
	ssh.KeyAlgoED25519,
	ssh.KeyAlgoECDSA256,
	ssh.KeyAlgoECDSA384,
	ssh.KeyAlgoECDSA521,
	ssh.KeyAlgoSKED25519,
	ssh.KeyAlgoSKECDSA256,
	ssh.KeyAlgoRSASHA512,
	ssh.KeyAlgoRSASHA256,
}

// resolveAuth returns the appropriate auth method for a repo.
// HTTPS public repos use anonymous (nil) auth; SSH repos use key-based auth
// with built-in host key verification (plus any extra entries from
// ssh_known_hosts).
func resolveAuth(repo *config.RepoConfig) (transport.AuthMethod, error) {
	if repo.IsHTTPS() {
		return nil, nil
	}
	// Integration test bypass: allow local bare repos to simulate remotes without SSH.
	if strings.HasPrefix(repo.URL, "/") && flag.Lookup("test.v") != nil {
		return nil, nil
	}
	hostPort, err := sshHostPort(repo.URL)
	if err != nil {
		return nil, fmt.Errorf("parsing SSH endpoint %q: %w", repo.URL, err)
	}
	return sshAuth(hostPort, repo.SSHKeyPath, repo.SSHKnownHosts)
}

// sshAuth creates an SSH public key auth method from a key file.
// Host key verification uses built-in known_hosts for major providers merged
// with any extra user-provided entries; HostKeyAlgorithms is the OpenSSH
// default order with the algorithms we have entries for at hostPort promoted
// to the front (see preferredHostKeyAlgorithms and mergeAlgorithms).
func sshAuth(hostPort, keyPath, extraKnownHosts string) (*gitssh.PublicKeys, error) {
	auth, err := gitssh.NewPublicKeysFromFile("git", keyPath, "")
	if err != nil {
		return nil, fmt.Errorf("loading SSH key %s: %w", keyPath, err)
	}

	callback, hostAlgos, err := buildKnownHostsAuth(extraKnownHosts, hostPort)
	if err != nil {
		return nil, fmt.Errorf("building known_hosts auth: %w", err)
	}
	auth.HostKeyCallback = callback
	auth.HostKeyAlgorithms = mergeAlgorithms(hostAlgos, preferredHostKeyAlgorithms)

	return auth, nil
}

// sshHostPort returns the "host:port" string for the SSH connection implied
// by repoURL. Uses transport.NewEndpoint so both URL-form
// ("ssh://git@host:port/path") and scp-form ("git@host:path") parse
// consistently; falls back to port 22 when none is specified.
func sshHostPort(repoURL string) (string, error) {
	ep, err := transport.NewEndpoint(repoURL)
	if err != nil {
		return "", err
	}
	port := ep.Port
	if port == 0 {
		port = defaultSSHPort
	}
	return ep.Host + ":" + strconv.Itoa(port), nil
}

// mergeAlgorithms returns hostAlgos concatenated with fallback, deduplicated,
// preserving the order of first appearance. The "first appearance wins"
// rule means an algorithm present in hostAlgos keeps its earlier position
// instead of being shadowed by the same algorithm later in fallback.
//
// This is the dynamic-promotion step of mimicking OpenSSH's
// HostKeyAlgorithms behaviour: for the specific host:port we're dialing,
// surface whatever algorithms our known_hosts already has entries for so
// negotiation lands on a key type we can verify.
func mergeAlgorithms(hostAlgos, fallback []string) []string {
	seen := make(map[string]struct{}, len(hostAlgos)+len(fallback))
	out := make([]string, 0, len(hostAlgos)+len(fallback))
	for _, a := range hostAlgos {
		if _, ok := seen[a]; ok {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	for _, a := range fallback {
		if _, ok := seen[a]; ok {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	return out
}
