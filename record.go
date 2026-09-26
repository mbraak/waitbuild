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
type recorder struct {
	dir string
	w   watch.Watch
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
	r.save()
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
	r.save()
}

// finish records that every check completed; url is the page to open.
func (r *recorder) finish(ok bool, url string) {
	if r == nil {
		return
	}
	r.w.Finished = true
	r.w.OK = ok
	if url != "" {
		r.w.URL = url
	}
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

func (r *recorder) save() {
	r.w.Updated = time.Now()
	_ = watch.Save(r.dir, r.w)
}
