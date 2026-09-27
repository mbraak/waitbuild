package main

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/mbraak/waitbuild/internal/watch"
)

// fakeStarts records the waitbuild invocations of a rerunWatcher. Each one
// runs until release is called for it.
type fakeStarts struct {
	mu      sync.Mutex
	args    [][]string
	release []chan struct{}
	err     error
}

func (f *fakeStarts) watcher(interval time.Duration) *rerunWatcher {
	r := newRerunWatcher("waitbuild", interval)
	r.start = func(args []string) (func() error, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.err != nil {
			return nil, f.err
		}
		ch := make(chan struct{})
		f.args = append(f.args, args)
		f.release = append(f.release, ch)
		return func() error { <-ch; return nil }, nil
	}
	return r
}

func (f *fakeStarts) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.args)
}

func TestRerunWatcherChecksBuildsThatDidNotSucceed(t *testing.T) {
	var f fakeStarts
	r := f.watcher(time.Minute)
	// Each build in its own repository, so that none supersedes another.
	failed := finished("aaa", false, now)
	errored := finished("bbb", false, now)
	errored.Repo = "r2"
	errored.Error = "timeout"
	stopped := running("ccc", now)
	stopped.Repo = "r3"
	stopped.PID = 0
	succeeded := finished("ddd", true, now)
	succeeded.Repo = "r4"
	busy := running("eee", now)
	busy.Repo = "r5"
	watches := []watch.Watch{failed, errored, stopped, succeeded, busy}

	r.check(watches, now, func() {})
	if f.count() != 3 {
		t.Fatalf("started %d checks, want 3 (failed, errored, stopped): %v", f.count(), f.args)
	}
	want := []string{"-quiet", "-notify", "-if-rerun", "-repo", "o/r", "-sha", "aaa", "-branch", "feature"}
	if !reflect.DeepEqual(f.args[0], want) {
		t.Errorf("args = %v, want %v", f.args[0], want)
	}
}

func TestRerunWatcherSkipsSupersededBuilds(t *testing.T) {
	var f fakeStarts
	r := f.watcher(time.Minute)
	older := finished("aaa", false, now.Add(-time.Hour))
	newer := finished("bbb", false, now)
	otherRepo := finished("ccc", false, now.Add(-time.Hour))
	otherRepo.Repo = "other"
	// A newer build supersedes an older one whatever its state.
	olderThanRunning := finished("ddd", false, now.Add(-time.Hour))
	olderThanRunning.Repo = "busy"
	busy := running("eee", now)
	busy.Repo = "busy"

	r.check([]watch.Watch{newer, older, otherRepo, busy, olderThanRunning}, now, func() {})
	var shas []string
	for _, args := range f.args {
		shas = append(shas, args[6])
	}
	if want := []string{"bbb", "ccc"}; !reflect.DeepEqual(shas, want) {
		t.Errorf("checked %v, want %v", shas, want)
	}
}

func TestRerunWatcherInterval(t *testing.T) {
	var f fakeStarts
	r := f.watcher(time.Minute)
	watches := []watch.Watch{finished("aaa", false, now)}
	done := make(chan struct{}, 1)

	r.check(watches, now, func() { done <- struct{}{} })
	r.check(watches, now.Add(2*time.Minute), func() {})
	if f.count() != 1 {
		t.Fatalf("started %d checks, want 1 while the first is still running", f.count())
	}

	close(f.release[0])
	<-done
	r.check(watches, now.Add(30*time.Second), func() {})
	if f.count() != 1 {
		t.Fatalf("started %d checks, want 1 within the interval", f.count())
	}
	r.check(watches, now.Add(time.Minute), func() {})
	if f.count() != 2 {
		t.Fatalf("started %d checks, want 2 after the interval", f.count())
	}
	close(f.release[1])
}

func TestRerunWatcherStartError(t *testing.T) {
	f := fakeStarts{err: errors.New("no such file")}
	r := f.watcher(time.Minute)
	r.check([]watch.Watch{finished("aaa", false, now)}, now, func() { t.Error("done called for a check that did not start") })
	if len(r.active) != 0 {
		t.Errorf("active = %v, want empty", r.active)
	}
	if _, ok := r.checked["o/r@aaa"]; !ok {
		t.Error("a failed start should still count as a check, so it is not retried on every refresh")
	}
}

func TestRerunWatcherForgetsDismissedBuilds(t *testing.T) {
	f := fakeStarts{err: errors.New("fail fast")}
	r := f.watcher(time.Minute)
	r.check([]watch.Watch{finished("aaa", false, now)}, now, func() {})
	r.check(nil, now, func() {})
	if len(r.checked) != 0 {
		t.Errorf("checked = %v, want dismissed build forgotten", r.checked)
	}
}
