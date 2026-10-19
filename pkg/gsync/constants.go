package gsync

// RemoteOrigin is the default remote name used for git operations.
const (
	RemoteOrigin     = "origin"
	DefaultTestName  = "test"
	DevelopBranch    = "develop"
	MainBranch       = "main"
	MasterBranch     = "master"
	DefaultTag       = "v1.0.0"
	DefaultTestEmail = "test@test.com"
	Two              = 2
	FeatureAuth      = "feature-auth"
	OldBranch        = "old-branch"
)

// Failure codes returned by Classify. They are the stable, machine-readable
// error surface of the daemon HTTP API.
const (
	CodeSecretUnavailable  FailureCode = "secret_unavailable"
	CodeSecretForbidden    FailureCode = "secret_forbidden"
	CodeSecretNotFound     FailureCode = "secret_not_found"
	CodeSecretInvalid      FailureCode = "secret_invalid"
	CodeSecretRejected     FailureCode = "secret_rejected"
	CodeGitAuthFailed      FailureCode = "git_auth_failed"
	CodeGitRepoNotFound    FailureCode = "git_repo_not_found"
	CodeGitHostKeyMismatch FailureCode = "git_host_key_mismatch"
	CodeGitUnreachable     FailureCode = "git_unreachable"
	CodeInternal           FailureCode = "internal"
)

// goGitTimeoutExceeded is the text of go-git's internal
// common.ErrTimeoutExceeded, which is not importable.
const goGitTimeoutExceeded = "timeout exceeded"
