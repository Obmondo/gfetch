package gsync

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/obmondo/gfetch/pkg/config"
)

// Azure DevOps hosts. go-git cannot fetch objects from these: the fetch
// completes, the refs are written, and the object store is left empty — no
// error, no pack. Every shape we tried behaves the same (narrow refspec,
// wildcard refspec, and both PlainClone variants), so this is not something
// gfetch can work around by asking differently. Shell out to the git binary
// for these remotes instead, which negotiates a protocol Azure DevOps serves
// correctly and returns a complete pack.
// git fetch flags, hoisted so the repeated literals stay in one place.
const (
	flagNoTags = "--no-tags"
	flagForce  = "--force"
)

var execFetchHostSuffixes = []string{
	"ssh.dev.azure.com",
	"vs-ssh.visualstudio.com",
	"dev.azure.com",
	".visualstudio.com",
}

// needsExecFetch reports whether a repo URL points at a host that go-git
// cannot fetch from, and must therefore be fetched with the git binary.
func needsExecFetch(rawURL string) bool {
	host := sshHostOnly(rawURL)
	if host == "" {
		return false
	}
	host = strings.ToLower(host)
	for _, suffix := range execFetchHostSuffixes {
		if host == strings.TrimPrefix(suffix, ".") || strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// sshHostOnly extracts the host from an ssh:// or scp-style git URL, dropping
// any user and port. Returns "" when the URL is not SSH.
func sshHostOnly(rawURL string) string {
	hostPort, err := sshHostPort(rawURL)
	if err != nil {
		return ""
	}
	if idx := strings.LastIndex(hostPort, ":"); idx != -1 {
		return hostPort[:idx]
	}
	return hostPort
}

// gitSSHCommand builds the GIT_SSH_COMMAND the exec'd git uses, pinning it to
// the repo's key and to a known_hosts file holding the same entries go-git
// would have verified against. IdentitiesOnly stops ssh from silently trying
// an agent key that happens to be loaded.
func gitSSHCommand(repo *config.RepoConfig) (cmd string, cleanup func(), err error) {
	knownHostsPath, cleanup, err := writeKnownHostsFile(repo.SSHKnownHosts)
	if err != nil {
		return "", func() {}, err
	}

	parts := []string{
		"ssh",
		"-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", fmt.Sprintf("UserKnownHostsFile=%s", knownHostsPath),
	}
	if repo.SSHKeyPath != "" {
		parts = append(parts, "-i", repo.SSHKeyPath)
	}
	return strings.Join(parts, " "), cleanup, nil
}

// execFetch runs a fetch through the git binary, in the same repo go-git would
// have used. flags and refSpecs mirror whatever the go-git call passed, so the
// refs that land are identical and the rest of the sync is unchanged.
//
// workDir is the repo to run in - usually repo.LocalPath, but the openvox
// resolver and staleness checks operate on other directories.
func execFetch(ctx context.Context, repo *config.RepoConfig, workDir string, flags, refSpecs []string) error {
	sshCmd, cleanup, err := gitSSHCommand(repo)
	defer cleanup()
	if err != nil {
		return fmt.Errorf("building ssh command: %w", err)
	}

	args := append([]string{"fetch"}, flags...)
	args = append(args, RemoteOrigin)
	args = append(args, refSpecs...)

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		"GIT_SSH_COMMAND="+sshCmd,
		// Never block on a credential or passphrase prompt in a daemon.
		"GIT_TERMINAL_PROMPT=0",
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git fetch %v: %w: %s", refSpecs, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// execFetchBranch mirrors syncBranch's fetch.
func execFetchBranch(ctx context.Context, repo *config.RepoConfig, branch string) error {
	refSpec := fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", branch, RemoteOrigin, branch)
	return execFetch(ctx, repo, repo.LocalPath, []string{flagNoTags, flagForce}, []string{refSpec})
}

// execFetchTags mirrors fetchTags. Tags carry objects like any other ref, so
// they hit the same wall on Azure DevOps as branches do.
func execFetchTags(ctx context.Context, repo *config.RepoConfig, tags []string) error {
	refSpecs := make([]string, len(tags))
	for i, tag := range tags {
		refSpecs[i] = fmt.Sprintf("+refs/tags/%s:refs/tags/%s", tag, tag)
	}
	return execFetch(ctx, repo, repo.LocalPath, []string{flagNoTags}, refSpecs)
}
