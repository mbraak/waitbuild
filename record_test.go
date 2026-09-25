package main

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/go-github/v90/github"
	"github.com/mbraak/waitbuild/internal/watch"
)

// TestMain keeps the tests from writing watch files into the user's cache
// directory, where a running waitbuild-menubar would show them.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "waitbuild-state-")
	if err != nil {
		panic(err)
	}
	os.Setenv("WAITBUILD_STATE_DIR", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// loadOnly returns the single watch recorded in dir.
func loadOnly(t *testing.T, dir string) watch.Watch {
	t.Helper()
	watches, err := watch.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(watches) != 1 {
		t.Fatalf("recorded %d watches, want 1: %+v", len(watches), watches)
	}
	return watches[0]
}

func TestRunRecordsWatch(t *testing.T) {
	const remote = "https://github.com/mbraak/waitbuild.git"
	const interval = time.Millisecond
	t.Setenv("PATH", t.TempDir()) // no gh, no osascript
	t.Setenv("GITHUB_TOKEN", "tok")
	t.Setenv("GH_TOKEN", "")

	t.Run("records every poll and the result", func(t *testing.T) {
		state := t.TempDir()
		t.Setenv("WAITBUILD_STATE_DIR", state)
		dir, _, sha := initRepo(t, remote)
		t.Chdir(dir)
		f := newFakeAPI(t,
			&poll{runs: []*github.WorkflowRun{mkRun(1, "build", "in_progress", "")}},
			&poll{
				runs:     []*github.WorkflowRun{done(1, "build", "failure")},
				statuses: []*github.RepoStatus{mkStatus("ci/circleci: lint", "success")},
			},
		)
		f.pulls = []*github.PullRequest{{State: github.Ptr("open"), HTMLURL: github.Ptr("https://github.com/mbraak/waitbuild/pull/7")}}
		useFakeAPI(t, f, "tok")

		captureStdout(t, func() {
			if err := run("", "feature", "", false, interval, time.Minute, time.Minute, false); err == nil {
				t.Error("run() = nil, want failure")
			}
		})

		w := loadOnly(t, state)
		if w.Owner != "mbraak" || w.Repo != "waitbuild" || w.Branch != "feature" || w.SHA != sha {
			t.Errorf("watch = %s/%s %s %s, want mbraak/waitbuild feature %s", w.Owner, w.Repo, w.Branch, w.SHA, sha)
		}
		if w.PID != os.Getpid() {
			t.Errorf("PID = %d, want %d", w.PID, os.Getpid())
		}
		if !w.Finished || w.OK || w.Error != "" {
			t.Errorf("finished=%v ok=%v error=%q, want a finished failed build", w.Finished, w.OK, w.Error)
		}
		if got := w.State(); got != watch.Failure {
			t.Errorf("State() = %s, want %s", got, watch.Failure)
		}
		if w.URL != "https://github.com/mbraak/waitbuild/pull/7" {
			t.Errorf("URL = %q, want the pull request", w.URL)
		}
		want := []watch.Check{
			{Name: "build", Status: "completed", Conclusion: "failure", URL: "https://github.com/o/r/actions/runs/1"},
			{Name: "ci/circleci: lint", Status: "completed", Conclusion: "success", URL: "https://circleci.com/gh/o/r/ci/circleci:lint"},
		}
		if len(w.Checks) != len(want) {
			t.Fatalf("checks = %+v, want %+v", w.Checks, want)
		}
		for i := range want {
			if w.Checks[i] != want[i] {
				t.Errorf("check %d = %+v, want %+v", i, w.Checks[i], want[i])
			}
		}
	})

	t.Run("records why it gave up", func(t *testing.T) {
		state := t.TempDir()
		t.Setenv("WAITBUILD_STATE_DIR", state)
		dir, _, sha := initRepo(t, remote)
		t.Chdir(dir)
		f := newFakeAPI(t, &poll{})
		useFakeAPI(t, f, "tok")

		captureStdout(t, func() {
			if err := run("", "", "", false, interval, 20*time.Millisecond, time.Minute, false); err == nil {
				t.Error("run() = nil, want no-checks error")
			}
		})

		w := loadOnly(t, state)
		if !w.Finished || !strings.Contains(w.Error, "no checks appeared") {
			t.Errorf("finished=%v error=%q, want the no-checks error", w.Finished, w.Error)
		}
		if got := w.State(); got != watch.Error {
			t.Errorf("State() = %s, want %s", got, watch.Error)
		}
		if want := "https://github.com/mbraak/waitbuild/commit/" + sha + "/checks"; w.URL != want {
			t.Errorf("URL = %q, want %q", w.URL, want)
		}
	})
}

func TestNilRecorder(t *testing.T) {
	var r *recorder
	r.update([]check{{name: "build"}})
	r.finish(true, "https://example.com")
	r.fail(errors.New("boom"))
}
