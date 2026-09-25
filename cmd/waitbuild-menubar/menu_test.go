package main

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mbraak/waitbuild/internal/watch"
)

var now = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

// running returns a watch owned by this (live) process.
func running(sha string, started time.Time, checks ...watch.Check) watch.Watch {
	return watch.Watch{PID: os.Getpid(), Owner: "o", Repo: "r", Branch: "main", SHA: sha,
		URL: "https://github.com/o/r/commit/" + sha + "/checks", Started: started, Updated: now, Checks: checks}
}

// finished returns a watch whose process has exited after recording a result.
func finished(sha string, ok bool, updated time.Time, checks ...watch.Check) watch.Watch {
	return watch.Watch{Owner: "o", Repo: "r", Branch: "feature", SHA: sha, URL: "https://github.com/o/r/pull/1",
		Started: updated.Add(-time.Minute), Updated: updated, Finished: true, OK: ok, Checks: checks}
}

func titles(items []item) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.title)
	}
	return out
}

func TestBuildMenuIdle(t *testing.T) {
	m := buildMenu(nil, now)
	if m.title != idleTitle || len(m.entries) != 0 || m.hasFinished {
		t.Errorf("buildMenu(nil) = %+v, want idle menu", m)
	}
}

func TestBuildMenuRunning(t *testing.T) {
	m := buildMenu([]watch.Watch{
		running("1111111111", now.Add(-3*time.Minute),
			watch.Check{Name: "build", Status: "completed", Conclusion: "success", URL: "https://x/build"},
			watch.Check{Name: "lint", Status: "in_progress", URL: "https://x/lint"},
		),
		running("2222222222", now.Add(-10*time.Second)),
		finished("3333333333", false, now.Add(-time.Hour)),
	}, now)

	if m.title != "⏳ 2" {
		t.Errorf("title = %q, want ⏳ 2", m.title)
	}
	if !m.hasFinished {
		t.Error("hasFinished = false with a finished build")
	}
	if want := "⏳ o/r · main · 1111111 — 1/2, running 3m"; m.entries[0].title != want {
		t.Errorf("entry title = %q, want %q", m.entries[0].title, want)
	}
	wantItems := []item{
		{title: "Open on GitHub", url: "https://github.com/o/r/commit/1111111111/checks"},
		{title: "✔ build — success", url: "https://x/build"},
		{title: "⏳ lint — in_progress", url: "https://x/lint"},
	}
	if !reflect.DeepEqual(m.entries[0].items, wantItems) {
		t.Errorf("items = %+v, want %+v", m.entries[0].items, wantItems)
	}
	if want := "⏳ o/r · main · 2222222 — 0/0, just started"; m.entries[1].title != want {
		t.Errorf("entry title = %q, want %q", m.entries[1].title, want)
	}
	if got := titles(m.entries[1].items); !reflect.DeepEqual(got, []string{"Open on GitHub", "Waiting for checks to appear…"}) {
		t.Errorf("items without checks = %v", got)
	}
}

func TestBuildMenuFinished(t *testing.T) {
	failed := finished("aaaaaaaaaa", false, now.Add(-5*time.Minute),
		watch.Check{Name: "build", Status: "completed", Conclusion: "failure", URL: "https://x/build"})
	m := buildMenu([]watch.Watch{failed, finished("bbbbbbbbbb", true, now.Add(-2*time.Hour))}, now)

	if m.title != "❌" {
		t.Errorf("title = %q, want the most recent result ❌", m.title)
	}
	e := m.entries[0]
	if want := "❌ o/r · feature · aaaaaaa — 5m ago"; e.title != want {
		t.Errorf("entry title = %q, want %q", e.title, want)
	}
	if got := titles(e.items); !reflect.DeepEqual(got, []string{"Open on GitHub", "✘ build — failure", "Dismiss"}) {
		t.Errorf("items = %v", got)
	}
	if d := e.items[2].dismiss; d == nil || d.SHA != failed.SHA {
		t.Errorf("Dismiss item dismisses %+v, want the failed build", d)
	}
	if want := "✅ o/r · feature · bbbbbbb — 2h ago"; m.entries[1].title != want {
		t.Errorf("entry title = %q, want %q", m.entries[1].title, want)
	}
}

func TestBuildMenuErrorAndStopped(t *testing.T) {
	gaveUp := finished("aaaaaaaaaa", false, now)
	gaveUp.Error = "no checks appeared"
	stopped := running("bbbbbbbbbb", now.Add(-time.Hour))
	stopped.PID = 0 // no such process

	m := buildMenu([]watch.Watch{gaveUp, stopped}, now)
	if m.title != "⚠️" {
		t.Errorf("title = %q, want ⚠️", m.title)
	}
	if got := m.entries[0].items[1].title; got != "Gave up: no checks appeared" {
		t.Errorf("error item = %q", got)
	}
	if !strings.HasPrefix(m.entries[1].title, "⏹ ") {
		t.Errorf("stopped entry title = %q, want ⏹", m.entries[1].title)
	}
	if got := titles(m.entries[1].items); got[len(got)-1] != "Dismiss" {
		t.Errorf("stopped build should be dismissable: %v", got)
	}
}

func TestBuildMenuCapsEntries(t *testing.T) {
	var watches []watch.Watch
	for i := 0; i < maxEntries+5; i++ {
		watches = append(watches, finished(strings.Repeat("a", 10), true, now))
	}
	if got := len(buildMenu(watches, now).entries); got != maxEntries {
		t.Errorf("entries = %d, want %d", got, maxEntries)
	}
}

func TestShort(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Second: "",
		90 * time.Second: "1m",
		2 * time.Hour:    "2h",
		50 * time.Hour:   "2d",
	} {
		if got := short(d); got != want {
			t.Errorf("short(%s) = %q, want %q", d, got, want)
		}
	}
}
