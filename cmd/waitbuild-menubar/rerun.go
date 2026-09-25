package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/mbraak/waitbuild/internal/watch"
)

// rerunWatcher picks up builds that were rerun on GitHub after waitbuild
// reported them. The watch file of a finished build no longer changes, so for
// every build that did not succeed it periodically starts
// `waitbuild -if-rerun`: that exits right away while the build is unchanged,
// and otherwise waits for it like any other build, rewriting the watch file so
// the menu shows it running again, or its new result when the rerun already
// finished.
type rerunWatcher struct {
	waitbuild string        // path of the waitbuild binary
	interval  time.Duration // how often to check each build
	start     func(args []string) (wait func() error, err error)

	mu      sync.Mutex
	checked map[string]time.Time // watch key -> when the last check started
	active  map[string]bool      // watch key -> a check is running
}

func newRerunWatcher(waitbuild string, interval time.Duration) *rerunWatcher {
	return &rerunWatcher{
		waitbuild: waitbuild,
		interval:  interval,
		start: func(args []string) (func() error, error) {
			cmd := exec.Command(waitbuild, args...)
			cmd.Stderr = os.Stderr
			if err := cmd.Start(); err != nil {
				return nil, err
			}
			return cmd.Wait, nil
		},
		checked: map[string]time.Time{},
		active:  map[string]bool{},
	}
}

// findWaitbuild returns the waitbuild binary next to this executable, or the
// one on PATH.
func findWaitbuild() (string, error) {
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "waitbuild")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	return exec.LookPath("waitbuild")
}

func watchKey(w watch.Watch) string { return w.Owner + "/" + w.Repo + "@" + w.SHA }

// rerunnable reports whether w is worth checking for a rerun: it finished
// without success, or waitbuild stopped before it finished.
func rerunnable(w watch.Watch) bool {
	switch w.State() {
	case watch.Failure, watch.Error, watch.Stopped:
		return true
	}
	return false
}

// check starts a rerun check for every rerunnable watch that was not checked
// within the interval. done is called after each check.
func (r *rerunWatcher) check(watches []watch.Watch, now time.Time, done func()) {
	r.mu.Lock()
	defer r.mu.Unlock()

	current := map[string]bool{}
	for _, w := range watches {
		key := watchKey(w)
		current[key] = true
		if !rerunnable(w) || r.active[key] || now.Sub(r.checked[key]) < r.interval {
			continue
		}
		args := []string{"-quiet", "-notify", "-if-rerun",
			"-repo", w.Owner + "/" + w.Repo, "-sha", w.SHA, "-branch", w.Branch}
		wait, err := r.start(args)
		r.checked[key] = now
		if err != nil {
			fmt.Fprintln(os.Stderr, "waitbuild-menubar: starting", r.waitbuild+":", err)
			continue
		}
		r.active[key] = true
		go func() {
			_ = wait() // waitbuild reports its own errors on stderr
			r.mu.Lock()
			delete(r.active, key)
			r.mu.Unlock()
			done()
		}()
	}
	// Forget dismissed builds, so that the maps do not grow forever.
	for key := range r.checked {
		if !current[key] && !r.active[key] {
			delete(r.checked, key)
		}
	}
}
