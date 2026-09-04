package gsync

import (
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// go-git ships with MultiACK, MultiACKDetailed and ThinPack all declared
// unsupported, so it never advertises them during fetch negotiation. Azure
// DevOps answers a client that offers no multi_ack with an *empty pack* rather
// than an error: the fetch returns nil, the refs are written, and the object
// store is left empty. Checkout then fails with "object not found", and because
// the refs already point at the right commits nothing ever repairs itself.
//
// Re-enabling the multi_ack capabilities makes Azure DevOps send a real pack.
// Verified against ebillet's repo: with the stock list a fetch produced zero
// objects, with this one it produced the same 172K pack the git binary sends.
//
// ThinPack stays disabled - unlike the other two that reflects a real gap in
// go-git's pack handling, not a negotiation preference.
//
// This is process-wide, not per-remote: transport.UnsupportedCapabilities is a
// package-level variable in go-git. Advertising multi_ack is what every normal
// git client does, so it is safe for the other hosts we sync from.
func init() {
	transport.UnsupportedCapabilities = []capability.Capability{
		capability.ThinPack,
	}
}
