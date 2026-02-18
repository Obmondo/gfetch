package config

import (
	"fmt"
	"net/http"
	"strings"
)

// CheckHTTPSAccessible verifies that an HTTPS repo URL is publicly reachable.
// Returns a non-nil error (with a human-friendly message) if not.
func CheckHTTPSAccessible(repoName, rawURL string) error {
	checkURL := strings.TrimSuffix(rawURL, ".git")
	resp, err := http.Head(checkURL)
	if err != nil {
		return fmt.Errorf("repo %s: HTTPS URL is not reachable: %w", repoName, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("repo %s: HTTPS URL is not publicly accessible (status %d); "+
			"only public repos are supported with HTTPS — use an SSH URL with ssh_key_path for private repos",
			repoName, resp.StatusCode)
	}
	return nil
}
