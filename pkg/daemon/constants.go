package daemon

import (
	"net/http"

	"github.com/obmondo/gfetch/pkg/gsync"
)

// Daemon-only failure codes, alongside the gsync.Classify codes in the HTTP
// error contract.
const (
	codeShuttingDown      gsync.FailureCode = "shutting_down"
	codeSyncInProgress    gsync.FailureCode = "sync_in_progress"
	codeRepoNotFound      gsync.FailureCode = "repo_not_found"
	codeConfigLoadFailed  gsync.FailureCode = "config_load_failed"
	codeConfigInvalid     gsync.FailureCode = "config_invalid"
	codeConfigApplyFailed gsync.FailureCode = "config_apply_failed"
)

type failureSpec struct {
	status  int
	message string
}

// failureSpecs is the single source of each code's HTTP status and message.
var failureSpecs = map[gsync.FailureCode]failureSpec{
	gsync.CodeSecretUnavailable:  {http.StatusServiceUnavailable, "secret fetch failed: openbao unavailable"},
	gsync.CodeSecretForbidden:    {http.StatusServiceUnavailable, "secret fetch failed: access denied"},
	gsync.CodeSecretNotFound:     {http.StatusServiceUnavailable, "secret fetch failed: secret not found"},
	gsync.CodeSecretInvalid:      {http.StatusServiceUnavailable, "secret fetch failed: invalid ssh key"},
	gsync.CodeSecretRejected:     {http.StatusServiceUnavailable, "secret fetch failed: request rejected"},
	gsync.CodeGitAuthFailed:      {http.StatusUnprocessableEntity, "git clone failed: authentication rejected by git host"},
	gsync.CodeGitRepoNotFound:    {http.StatusUnprocessableEntity, "git clone failed: repository not found or no access"},
	gsync.CodeGitHostKeyMismatch: {http.StatusUnprocessableEntity, "git clone failed: host key verification failed"},
	gsync.CodeGitUnreachable:     {http.StatusUnprocessableEntity, "git clone failed: git host unreachable"},
	gsync.CodeInternal:           {http.StatusInternalServerError, "sync failed: internal error"},
	codeShuttingDown:             {http.StatusServiceUnavailable, errDaemonShuttingDown.Error()},
	codeSyncInProgress:           {http.StatusConflict, errSyncInProgress.Error()},
	codeRepoNotFound:             {http.StatusNotFound, "repo not found"},
	codeConfigLoadFailed:         {http.StatusInternalServerError, "config load failed"},
	codeConfigInvalid:            {http.StatusUnprocessableEntity, "config invalid"},
	codeConfigApplyFailed:        {http.StatusInternalServerError, "config apply failed"},
}

// specFor returns the code's table row; a missing code falls back to the
// internal row.
func specFor(c gsync.FailureCode) failureSpec {
	if s, ok := failureSpecs[c]; ok {
		return s
	}
	return failureSpecs[gsync.CodeInternal]
}

func statusFor(c gsync.FailureCode) int { return specFor(c).status }

func messageFor(c gsync.FailureCode) string { return specFor(c).message }
