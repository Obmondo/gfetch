package gsync

import (
	"testing"

	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// TestMultiACKCapabilitiesEnabled pins the Azure DevOps fix. go-git's stock
// UnsupportedCapabilities withholds multi_ack, and Azure DevOps answers such a
// client with an empty pack instead of an error - a fetch that returns nil and
// leaves no objects behind. If this list regains MultiACK, those repos silently
// stop syncing again.
func TestMultiACKCapabilitiesEnabled(t *testing.T) {
	for _, c := range []capability.Capability{capability.MultiACK, capability.MultiACKDetailed} {
		for _, unsupported := range transport.UnsupportedCapabilities {
			if unsupported == c {
				t.Errorf("%s must stay advertised: Azure DevOps sends an empty pack without it", c)
			}
		}
	}

	// ThinPack is a real gap in go-git's pack handling, so it stays disabled.
	found := false
	for _, unsupported := range transport.UnsupportedCapabilities {
		if unsupported == capability.ThinPack {
			found = true
		}
	}
	if !found {
		t.Error("ThinPack should remain unsupported")
	}
}
