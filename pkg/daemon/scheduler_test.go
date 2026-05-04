package daemon

import (
	"log/slog"
	"testing"
	"time"

	"github.com/go-co-op/gocron/v2"

	"github.com/obmondo/gfetch/pkg/config"
	"github.com/obmondo/gfetch/pkg/gsync"
)

func TestNewScheduler(t *testing.T) {
	slog.Default()
	s := gsync.New()
	sched := NewScheduler(s, ":8080", "config.yaml")
	if sched == nil {
		t.Fatal("expected non-nil scheduler")
	}
	if sched.syncer != s {
		t.Error("syncer not set correctly")
	}
	if sched.ConfigPath() != "config.yaml" {
		t.Errorf("ConfigPath() = %q, want %q", sched.ConfigPath(), "config.yaml")
	}
}

func TestSchedulerReload_ReplacesJobs(t *testing.T) {
	sched := newTestScheduler(t)

	repoA := testRepo("a", "main")
	repoB := testRepo("b", "main")

	if err := sched.applyInitial(&config.Config{Repos: map[string]config.RepoConfig{"a": repoA}}); err != nil {
		t.Fatalf("applyInitial: %v", err)
	}
	if _, ok := sched.jobs["a"]; !ok {
		t.Fatalf("expected job for 'a' after applyInitial")
	}

	res, err := sched.Reload(&config.Config{Repos: map[string]config.RepoConfig{"b": repoB}})
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	assertEqualSlice(t, "repos", res.Repos, []string{"b"})

	if _, ok := sched.jobs["a"]; ok {
		t.Errorf("expected job 'a' to be removed")
	}
	if _, ok := sched.jobs["b"]; !ok {
		t.Errorf("expected job 'b' to be added")
	}
}

func TestSchedulerReload_ChangedRepoVisibleToNextFire(t *testing.T) {
	// This protects against the stale-closure bug: if Reload only updates the
	// hash without the task body re-reading from the atomic pointer, scheduled
	// fires would still see the old repo.
	sched := newTestScheduler(t)

	old := testRepo("a", "main")
	if err := sched.applyInitial(&config.Config{Repos: map[string]config.RepoConfig{"a": old}}); err != nil {
		t.Fatalf("applyInitial: %v", err)
	}

	updated := testRepo("a", "develop")
	if _, err := sched.Reload(&config.Config{Repos: map[string]config.RepoConfig{"a": updated}}); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	cur := sched.Config()
	gotBranch := cur.Repos["a"].Branches[0].Raw
	if gotBranch != "develop" {
		t.Errorf("scheduler config not updated: branch = %q, want %q", gotBranch, "develop")
	}
}

// TestSchedulerReload_DoesNotFireImmediately verifies that Reload's
// rescheduled jobs align their first run to one full poll_interval out, not
// to "now". This protects the upstream git host from a stampede when a single
// reload triggers a reschedule of every repo.
func TestSchedulerReload_DoesNotFireImmediately(t *testing.T) {
	sched := newTestScheduler(t)

	// Use a long initial interval so the initial applyInitial fire (which is
	// allowed to be immediate) doesn't muddy the post-reload observation.
	long := 10 * time.Minute
	rep := testRepo("a", "main")
	rep.PollInterval = config.Duration(long)
	if err := sched.applyInitial(&config.Config{Repos: map[string]config.RepoConfig{"a": rep}}); err != nil {
		t.Fatalf("applyInitial: %v", err)
	}

	// gocron only computes NextRun once the scheduler is running. Start it
	// here so the rescheduled job has a populated next-fire time we can
	// inspect. Cleanup is handled by newTestScheduler's deferred Shutdown.
	sched.cron.Start()

	newInterval := 5 * time.Minute
	updated := testRepo("a", "main")
	updated.PollInterval = config.Duration(newInterval)

	reloadAt := time.Now()
	if _, err := sched.Reload(&config.Config{Repos: map[string]config.RepoConfig{"a": updated}}); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	jobID := sched.jobs["a"]
	var job gocron.Job
	for _, j := range sched.cron.Jobs() {
		if j.ID() == jobID {
			job = j
			break
		}
	}
	if job == nil {
		t.Fatalf("rescheduled job %s not found", jobID)
	}

	next, err := job.NextRun()
	if err != nil {
		t.Fatalf("NextRun: %v", err)
	}

	// "Immediate" reschedule would set NextRun within milliseconds of reloadAt.
	// "Aligned to next tick" sets it ~newInterval from creation. Give a generous
	// margin around the lower bound to absorb test scheduling jitter.
	minNext := reloadAt.Add(newInterval - 30*time.Second)
	if next.Before(minNext) {
		t.Errorf("rescheduled job fires too soon: NextRun=%v, reloadAt=%v, want >= %v (interval %v)",
			next, reloadAt, minNext, newInterval)
	}
}

// newTestScheduler builds a Scheduler with a real gocron instance but does not
// start it. Tests can call applyInitial / Reload to exercise lifecycle logic.
func newTestScheduler(t *testing.T) *Scheduler {
	t.Helper()
	s := gsync.New()
	sched := NewScheduler(s, ":0", "")
	cron := newGocronForTest(t)
	sched.cron = cron
	t.Cleanup(func() { _ = cron.Shutdown() })
	return sched
}

func testRepo(name, branch string) config.RepoConfig {
	return config.RepoConfig{
		RepoDefaults: config.RepoDefaults{
			LocalPath:    "/tmp/" + name,
			PollInterval: config.Duration(time.Minute),
			Branches:     []config.Pattern{{Raw: branch}},
		},
		Name: name,
		URL:  "https://example.com/" + name + ".git",
	}
}

func newGocronForTest(t *testing.T) gocron.Scheduler {
	t.Helper()
	cron, err := gocron.NewScheduler()
	if err != nil {
		t.Fatalf("gocron.NewScheduler: %v", err)
	}
	return cron
}

func assertEqualSlice(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: len(got)=%d want=%d (got=%v want=%v)", label, len(got), len(want), got, want)
		return
	}
	gotSet := make(map[string]bool, len(got))
	for _, g := range got {
		gotSet[g] = true
	}
	for _, w := range want {
		if !gotSet[w] {
			t.Errorf("%s: missing %q in got=%v", label, w, got)
		}
	}
}
