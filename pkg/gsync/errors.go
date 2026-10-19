package gsync

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/obmondo/gfetch/pkg/secrets"
)

// FailureCode is a stable, machine-readable classification of a sync error.
type FailureCode string

// secretError is a failure to obtain a usable SSH key from the secret
// backend; code is fixed when it is created.
type secretError struct {
	code FailureCode
	err  error
}

func (e *secretError) Error() string { return e.err.Error() }
func (e *secretError) Unwrap() error { return e.err }

func newSecretError(repoName string, err error) *secretError {
	return &secretError{code: secretCode(err), err: fmt.Errorf("secret fetch failed for %s: %w", repoName, err)}
}

// secretCodes maps secrets sentinels to codes, checked in order.
var secretCodes = []struct {
	sentinel error
	code     FailureCode
}{
	{secrets.ErrNotFound, CodeSecretNotFound},
	{secrets.ErrForbidden, CodeSecretForbidden},
	{secrets.ErrUnavailable, CodeSecretUnavailable},
	{secrets.ErrInvalidSecret, CodeSecretInvalid},
	{secrets.ErrRejected, CodeSecretRejected},
}

// secretCode classifies a secret read error. A backend sentinel wins, so an
// OpenBao request timeout is secret_unavailable. A bare caller-context error
// (Store.Read returns ctx.Err() unwrapped) is internal. Anything else, such
// as a client config error, is secret_unavailable.
func secretCode(err error) FailureCode {
	for _, m := range secretCodes {
		if errors.Is(err, m.sentinel) {
			return m.code
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return CodeInternal
	}
	return CodeSecretUnavailable
}

// Classify maps a sync error to a FailureCode. It returns "" for a nil error
// and CodeInternal for anything it does not recognise.
func Classify(err error) FailureCode {
	// A secret error is checked first: a refetch failure joins it with the
	// ssh auth error that triggered the refetch.
	var se *secretError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &se):
		return se.code
	case errors.As(err, new(*knownhosts.KeyError)):
		return CodeGitHostKeyMismatch
	case errors.Is(err, transport.ErrRepositoryNotFound):
		return CodeGitRepoNotFound
	case isSSHAuthErr(err),
		errors.Is(err, transport.ErrAuthenticationRequired),
		errors.Is(err, transport.ErrAuthorizationFailed):
		return CodeGitAuthFailed
	case isNetErr(err), isGoGitTimeoutErr(err):
		return CodeGitUnreachable
	default:
		return CodeInternal
	}
}

// isNetErr reports whether err carries a network error. The bare
// context.DeadlineExceeded sentinel implements net.Error but is not a network
// error: the openvox file-lock wait (withOpenVoxLockTimeout) returns it while
// the caller ctx is still alive, so it is excluded and stays internal. A
// *net.OpError wrapping it (a dial timeout) still matches.
func isNetErr(err error) bool {
	var ne net.Error
	if !errors.As(err, &ne) {
		return false
	}
	bareDeadline := errors.Unwrap(ne) == nil && errors.Is(ne, context.DeadlineExceeded)
	return !bareDeadline
}

// isGoGitTimeoutErr reports whether err is go-git's timeout-exceeded error.
// It lives in an internal go-git package (common.ErrTimeoutExceeded) that
// cannot be imported, so substring matching is the only reliable detection.
func isGoGitTimeoutErr(err error) bool {
	return strings.Contains(err.Error(), goGitTimeoutExceeded)
}
