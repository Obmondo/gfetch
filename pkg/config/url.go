package config

import (
	"fmt"
	"net/http"
	"strings"
)

// checkHTTPSAccessible verifies that an HTTPS repo URL is publicly accessible.
func checkHTTPSAccessible(repoName, rawURL string) error {
	checkURL := strings.TrimSuffix(rawURL, ".git")
	resp, err := http.Head(checkURL)
	if err != nil || resp.StatusCode != http.StatusOK {
		return fmt.Errorf("repo %s: HTTPS URL is not publicly accessible; only public repos are supported with HTTPS — use an SSH URL with ssh_key_path for private repos", repoName)
	}
	resp.Body.Close()
	return nil
}
