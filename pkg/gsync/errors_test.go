package gsync

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/obmondo/gfetch/pkg/secrets"
)

const testNetTCP = "tcp"

func TestClassify(t *testing.T) {
	secretFetch := func(err error) error {
		return newSecretError("repo", err)
	}

	tests := []struct {
		name string
		err  error
		want FailureCode
	}{
		{name: "nil error", err: nil, want: ""},
		{name: "context canceled", err: context.Canceled, want: CodeInternal},
		{name: "secret fetch deadline exceeded", err: secretFetch(context.DeadlineExceeded), want: CodeInternal},
		{name: "secret unavailable", err: secretFetch(fmt.Errorf("%w (read status 503): boom", secrets.ErrUnavailable)), want: CodeSecretUnavailable},
		{name: "secret forbidden", err: secretFetch(fmt.Errorf("%w: denied after re-login for path %q", secrets.ErrForbidden, "p")), want: CodeSecretForbidden},
		{name: "secret not found", err: secretFetch(secrets.ErrNotFound), want: CodeSecretNotFound},
		{name: "secret invalid", err: secretFetch(fmt.Errorf("%w: field %q not present in secret at %q", secrets.ErrInvalidSecret, "f", "p")), want: CodeSecretInvalid},
		{name: "secret rejected", err: secretFetch(fmt.Errorf("%w (read status 400): boom", secrets.ErrRejected)), want: CodeSecretRejected},
		{name: "host key unknown", err: fmt.Errorf("ssh: handshake failed: %w", &knownhosts.KeyError{}), want: CodeGitHostKeyMismatch},
		{name: "host key mismatch", err: fmt.Errorf("ssh: handshake failed: %w", &knownhosts.KeyError{Want: []knownhosts.KnownKey{{Filename: "known_hosts", Line: 1}}}), want: CodeGitHostKeyMismatch},
		{name: "repository not found", err: fmt.Errorf("listing remote refs: listing remote refs: %w", transport.ErrRepositoryNotFound), want: CodeGitRepoNotFound},
		{name: "ssh unable to authenticate", err: errors.New("ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey]"), want: CodeGitAuthFailed},
		{name: "authentication required", err: transport.ErrAuthenticationRequired, want: CodeGitAuthFailed},
		{name: "connection refused", err: &net.OpError{Op: "dial", Net: testNetTCP, Err: syscall.ECONNREFUSED}, want: CodeGitUnreachable},
		{name: "openbao request timeout", err: secretFetch(fmt.Errorf("%w: %w", secrets.ErrUnavailable, &url.Error{Op: "Put", URL: "http://openbao:8200/v1/auth/kubernetes/login", Err: context.DeadlineExceeded})), want: CodeSecretUnavailable},
		{name: "git dial timeout", err: &net.OpError{Op: "dial", Net: testNetTCP, Err: context.DeadlineExceeded}, want: CodeGitUnreachable},
		{name: "git read deadline", err: &net.OpError{Op: "read", Net: testNetTCP, Err: os.ErrDeadlineExceeded}, want: CodeGitUnreachable},
		{name: "refetch failure joined with ssh auth error", err: errors.Join(errors.New("ssh: handshake failed: ssh: unable to authenticate"), secretFetch(secrets.ErrUnavailable)), want: CodeSecretUnavailable},
		{name: "openvox lock wait timeout", err: fmt.Errorf("waiting for file lock %s: %w", "/repo/.gfetch-meta/locks/main.lock", context.DeadlineExceeded), want: CodeInternal},
		{name: "other error", err: errors.New("checkout main: boom"), want: CodeInternal},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.err); got != tt.want {
				t.Errorf("Classify(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}
