package main

import (
	"fmt"
	"os"
	"time"

	"github.com/mbraak/waitbuild/internal/watch"
)

// recorder keeps the watch file of this process up to date, so that the
// waitbuild-menubar app can show the build. Recording is best effort: when
// the file cannot be written the build is still waited for and reported.
// A nil recorder records nothing.
//
// The file is only written once a check appears, or when waitbuild gives up:
// until then it is unknown whether the commit has a build at all (the
// workflows may not run for it), so the menu bar app should not show it as
// pending.
type recorder struct {
	dir   string
	w     watch.Watch
	saved bool // the watch file has been written
}

func newRecorder(owner, repo, branch, sha string) *recorder {
	dir, err := watch.Dir()
	if err != nil {
		return nil
	}
	now := time.Now()
	r := &recorder{dir: dir, w: watch.Watch{
		PID:     os.Getpid(),
		Owner:   owner,
		Repo:    repo,
		Branch:  branch,
		SHA:     sha,
		URL:     fmt.Sprintf("https://github.com/%s/%s/commit/%s/checks", owner, repo, sha),
		Started: now,
		Updated: now,
	}}
	return r
}

// update records the checks seen by the latest poll.
func (r *recorder) update(checks []check) {
	if r == nil {
		return
	}
	r.w.Checks = make([]watch.Check, len(checks))
	for i, c := range checks {
		r.w.Checks[i] = watch.Check{Name: c.name, Status: c.status, Conclusion: c.conclusion, URL: c.url}
	}
	if len(checks) > 0 || r.saved {
		r.save()
	}
}

// recordPolls returns an onPoll function for waitForChecks that records the
// checks of every poll. When the first check appears, it also records
// createPRURL(), so the menu offers to create a missing pull request while
// the build is still running.
func (r *recorder) recordPolls(createPRURL func() string) func([]check) {
	if r == nil {
		return nil
	}
	lookedUp := false
	return func(checks []check) {
		if !lookedUp && len(checks) > 0 {
			lookedUp = true
			r.w.CreatePullRequestURL = createPRURL()
		}
		r.update(checks)
	}
}

// finish records that every check completed; url is the page to open and
// createPRURL, when not empty, the page that creates a pull request.
func (r *recorder) finish(ok bool, url, createPRURL string) {
	if r == nil {
		return
	}
	r.w.Finished = true
	r.w.OK = ok
	if url != "" {
		r.w.URL = url
	}
	r.w.CreatePullRequestURL = createPRURL
	r.save()
}

// noChecks records that no check ran for the commit, but that a pull request
// can be created at createPRURL, which may start the workflows.
func (r *recorder) noChecks(createPRURL string) {
	if r == nil {
		return
	}
	r.w.Finished = true
	r.w.Checks = nil
	r.w.CreatePullRequestURL = createPRURL
	r.save()
}

// fail records that waitbuild gave up waiting.
func (r *recorder) fail(err error) {
	if r == nil {
		return
	}
	r.w.Finished = true
	r.w.Error = err.Error()
	r.save()
}

// remove deletes the watch file, so the build is not shown at all.
func (r *recorder) remove() {
	if r == nil {
		return
	}
	_ = watch.Remove(r.dir, r.w)
}

func (r *recorder) save() {
	r.w.Updated = time.Now()
	r.saved = true
	_ = watch.Save(r.dir, r.w)
}
