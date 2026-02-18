package daemon

import (
	"log/slog"
	"testing"

	"github.com/obmondo/gfetch/pkg/gsync"
)

func TestNewScheduler(t *testing.T) {
	logger := slog.Default()
	s := gsync.New(logger)
	sched := NewScheduler(s, logger, ":8080")
	if sched == nil {
		t.Fatal("expected non-nil scheduler")
	}
	if sched.syncer != s {
		t.Error("syncer not set correctly")
	}
	if sched.syncer != s {
		t.Error("syncer not set correctly")
	}
	if sched.logger != logger {
		t.Error("logger not set correctly")
	}
}
