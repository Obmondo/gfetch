package daemon

import (
	"log/slog"
	"testing"

	"github.com/obmondo/gfetch/pkg/gsync"
)

func TestNewScheduler(t *testing.T) {
	slog.Default()
	s := gsync.New()
	sched := NewScheduler(s, ":8080")
	if sched == nil {
		t.Fatal("expected non-nil scheduler")
	}
	if sched.syncer != s {
		t.Error("syncer not set correctly")
	}
	if sched.syncer != s {
		t.Error("syncer not set correctly")
	}

}
