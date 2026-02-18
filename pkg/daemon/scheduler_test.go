package daemon

import (
	"log/slog"
	"testing"

	"github.com/obmondo/gfetch/pkg/sync"
)

func TestNewScheduler(t *testing.T) {
	logger := slog.Default()
	syncer := sync.New(logger)
	sched := NewScheduler(syncer, logger, ":8080")
	if sched == nil {
		t.Fatal("expected non-nil scheduler")
	}
	if sched.syncer != syncer {
		t.Error("syncer not set correctly")
	}
	if sched.logger != logger {
		t.Error("logger not set correctly")
	}
}
