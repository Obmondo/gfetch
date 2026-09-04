package gsync

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestCheckoutRef_RepairsCorruptIndex pins the fix for repos stuck on
// "malformed index signature file". A truncated .git/index - a container killed
// mid-write - made every later checkout fail permanently, even though the index
// is pure cache that can be rebuilt from HEAD.
func TestCheckoutRef_RepairsCorruptIndex(t *testing.T) {
	dir := t.TempDir()
	r := initTestRepoWithCommitAtPath(t, dir)

	head, err := r.Head()
	if err != nil {
		t.Fatal(err)
	}
	branch := head.Name().Short()

	// Corrupt the index the way a killed process would: garbage where the DIRC
	// signature belongs.
	idx := filepath.Join(dir, ".git", "index")
	if err := os.WriteFile(idx, []byte("not-an-index"), 0644); err != nil {
		t.Fatal(err)
	}

	// Precondition: go-git really does refuse this index.
	if err := checkoutRefOnce(context.Background(), r, branch); !isMalformedIndexErr(err) {
		t.Fatalf("expected a malformed index error from the raw checkout, got %v", err)
	}

	// checkoutRefContext must repair it rather than propagate the failure. The
	// repair lives there, not in the checkoutRef wrapper, so the openvox path -
	// which calls checkoutRefContext directly - is covered too.
	if err := checkoutRefContext(context.Background(), r, branch); err != nil {
		t.Fatalf("checkoutRefContext should repair a corrupt index, got %v", err)
	}
}
