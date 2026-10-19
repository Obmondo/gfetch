package daemon

import (
	"net/http"

	"github.com/obmondo/gfetch/pkg/gsync"
)

// failureCode is a code in the HTTP error contract: gsync.Classify codes plus
// daemon-only ones.
type failureCode string

const (
	codeShuttingDown      failureCode = "shutting_down"
	codeSyncInProgress    failureCode = "sync_in_progress"
	codeRepoNotFound      failureCode = "repo_not_found"
	codeConfigLoadFailed  failureCode = "config_load_failed"
	codeConfigInvalid     failureCode = "config_invalid"
	codeConfigApplyFailed failureCode = "config_apply_failed"
)

type failureSpec struct {
	status  int
	message string
}

// failureSpecs is the single source of each code's HTTP status and message.
var failureSpecs = map[failureCode]failureSpec{
	failureCode(gsync.CodeSecretUnavailable):  {http.StatusServiceUnavailable, "secret fetch failed: openbao unavailable"},
	failureCode(gsync.CodeSecretForbidden):    {http.StatusServiceUnavailable, "secret fetch failed: access denied"},
	failureCode(gsync.CodeSecretNotFound):     {http.StatusServiceUnavailable, "secret fetch failed: secret not found"},
	failureCode(gsync.CodeSecretInvalid):      {http.StatusServiceUnavailable, "secret fetch failed: invalid ssh key"},
	failureCode(gsync.CodeSecretRejected):     {http.StatusServiceUnavailable, "secret fetch failed: request rejected"},
	failureCode(gsync.CodeGitAuthFailed):      {http.StatusUnprocessableEntity, "git clone failed: authentication rejected by git host"},
	failureCode(gsync.CodeGitRepoNotFound):    {http.StatusUnprocessableEntity, "git clone failed: repository not found or no access"},
	failureCode(gsync.CodeGitHostKeyMismatch): {http.StatusUnprocessableEntity, "git clone failed: host key verification failed"},
	failureCode(gsync.CodeGitUnreachable):     {http.StatusUnprocessableEntity, "git clone failed: git host unreachable"},
	failureCode(gsync.CodeInternal):           {http.StatusInternalServerError, "sync failed: internal error"},
	codeShuttingDown:                          {http.StatusServiceUnavailable, errDaemonShuttingDown.Error()},
	codeSyncInProgress:                        {http.StatusConflict, errSyncInProgress.Error()},
	codeRepoNotFound:                          {http.StatusNotFound, "repo not found"},
	codeConfigLoadFailed:                      {http.StatusInternalServerError, "config load failed"},
	codeConfigInvalid:                         {http.StatusUnprocessableEntity, "config invalid"},
	codeConfigApplyFailed:                     {http.StatusInternalServerError, "config apply failed"},
}

// spec returns the code's table row; a missing code falls back to the
// internal row.
func (c failureCode) spec() failureSpec {
	if s, ok := failureSpecs[c]; ok {
		return s
	}
	return failureSpecs[failureCode(gsync.CodeInternal)]
}

func (c failureCode) status() int { return c.spec().status }

func (c failureCode) message() string { return c.spec().message }
