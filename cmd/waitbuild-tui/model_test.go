package main

import (
	"os"
	"reflect"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
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

func texts(rows []row) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.text)
	}
	return out
}

// loaded returns a model showing watches.
func loaded(watches ...watch.Watch) model {
	m := newModel("", time.Hour, time.Second)
	next, _ := m.Update(loadedMsg{watches: watches, now: now})
	return next.(model)
}

func press(m model, keys ...string) (model, tea.Cmd) {
	var cmd tea.Cmd
	for _, k := range keys {
		var next tea.Model
		next, cmd = m.key(k)
		m = next.(model)
	}
	return m, cmd
}

func TestRowsExpandRunningBuilds(t *testing.T) {
	m := loaded(
		running("aaaaaaaaaa", now.Add(-2*time.Minute),
			watch.Check{Name: "build", Status: "completed", Conclusion: "success"},
			watch.Check{Name: "test", Status: "in_progress"}),
		finished("bbbbbbbbbb", false, now.Add(-5*time.Minute),
			watch.Check{Name: "lint", Status: "completed", Conclusion: "failure"}),
	)
	want := []string{
		"⏳ o/r · main · aaaaaaa — 1/2, running 2m",
		"✔ build — success",
		"⏳ test — in_progress",
		"❌ o/r · feature · bbbbbbb — 5m ago",
	}
	if got := texts(m.rows()); !reflect.DeepEqual(got, want) {
		t.Errorf("rows = %q, want %q", got, want)
	}

	m, _ = press(m, "down", "down", "down", "enter") // expand the finished build
	want = append(want, "✘ lint — failure")
	if got := texts(m.rows()); !reflect.DeepEqual(got, want) {
		t.Errorf("rows after expanding = %q, want %q", got, want)
	}

	m, _ = press(m, "home", "left") // collapse the running build
	if got := len(m.rows()); got != 3 {
		t.Errorf("rows after collapsing = %q, want 3 rows", texts(m.rows()))
	}
}

func TestRowsInfo(t *testing.T) {
	gaveUp := finished("cccccccccc", false, now)
	gaveUp.Error = "no checks appeared"
	stopped := running("dddddddddd", now)
	stopped.PID = 0
	m := loaded(running("aaaaaaaaaa", now), gaveUp, stopped)
	m, _ = press(m, "down", "down", "enter", "down", "down", "enter")
	want := []string{
		"⏳ o/r · main · aaaaaaa — 0/0, just started",
		"Waiting for checks to appear…",
		"⚠️ o/r · feature · ccccccc — just now",
		"Gave up: no checks appeared",
		"⏹ o/r · main · ddddddd — just now",
		"waitbuild exited before the build finished",
	}
	if got := texts(m.rows()); !reflect.DeepEqual(got, want) {
		t.Errorf("rows = %q, want %q", got, want)
	}
}

func TestSelectionSurvivesReload(t *testing.T) {
	a := finished("aaaaaaaaaa", true, now.Add(-time.Minute))
	b := finished("bbbbbbbbbb", false, now.Add(-2*time.Minute),
		watch.Check{Name: "lint", Status: "completed", Conclusion: "failure"})
	m := loaded(a, b)
	m, _ = press(m, "down", "enter", "down") // select b's check
	if m.cursor != 2 {
		t.Fatalf("cursor = %d, want 2", m.cursor)
	}

	// A new build appears at the top: the same check stays selected.
	next, _ := m.Update(loadedMsg{watches: []watch.Watch{running("cccccccccc", now), a, b}, now: now})
	m = next.(model)
	if r := m.rows()[m.cursor]; r.kind != checkRow || m.watches[r.watch].SHA != b.SHA {
		t.Errorf("after reload selected %q, want b's check", r.text)
	}

	// The check is gone: its build is selected.
	b.Checks = nil
	next, _ = m.Update(loadedMsg{watches: []watch.Watch{running("cccccccccc", now), a, b}, now: now})
	m = next.(model)
	if r := m.rows()[m.cursor]; r.kind != buildRow || m.watches[r.watch].SHA != b.SHA {
		t.Errorf("after the check disappeared selected %q, want b", r.text)
	}
}

func TestDismissAndClear(t *testing.T) {
	dir := t.TempDir()
	watches := []watch.Watch{
		running("aaaaaaaaaa", time.Now()),
		finished("bbbbbbbbbb", true, time.Now()),
		finished("cccccccccc", false, time.Now()),
	}
	for _, w := range watches {
		if err := watch.Save(dir, w); err != nil {
			t.Fatal(err)
		}
	}
	m := newModel(dir, time.Hour, time.Second)
	next, _ := m.Update(m.load(nil)())
	m = next.(model)

	// A running build cannot be dismissed.
	if _, cmd := press(m, "d"); cmd != nil {
		t.Error("dismissing a running build returned a command")
	}

	m, cmd := press(m, "down", "down", "d") // skip "Waiting for checks to appear…"
	next, _ = m.Update(cmd())
	m = next.(model)
	if got := shas(m.watches); !reflect.DeepEqual(got, []string{"aaaaaaaaaa", "cccccccccc"}) {
		t.Errorf("after dismiss = %v", got)
	}

	m, cmd = press(m, "c")
	next, _ = m.Update(cmd())
	m = next.(model)
	if got := shas(m.watches); !reflect.DeepEqual(got, []string{"aaaaaaaaaa"}) {
		t.Errorf("after clear = %v", got)
	}
}

func TestLoadRemovesOldBuilds(t *testing.T) {
	dir := t.TempDir()
	for _, w := range []watch.Watch{
		finished("aaaaaaaaaa", true, time.Now().Add(-2*time.Hour)),
		finished("bbbbbbbbbb", true, time.Now()),
	} {
		if err := watch.Save(dir, w); err != nil {
			t.Fatal(err)
		}
	}
	msg := newModel(dir, time.Hour, time.Second).load(nil)().(loadedMsg)
	if got := shas(msg.watches); !reflect.DeepEqual(got, []string{"bbbbbbbbbb"}) {
		t.Errorf("loaded %v, want only the recent build", got)
	}
	if left, _ := watch.Load(dir); len(left) != 1 {
		t.Errorf("%d watch files left, want 1", len(left))
	}
}

func TestScroll(t *testing.T) {
	var watches []watch.Watch
	for _, sha := range []string{"a1", "a2", "a3", "a4", "a5", "a6"} {
		watches = append(watches, finished(sha, true, now))
	}
	m := loaded(watches...)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 7}) // 3 rows fit
	m = next.(model)
	m, _ = press(m, "end")
	if m.cursor != 5 || m.offset != 3 {
		t.Errorf("at end cursor=%d offset=%d, want 5 and 3", m.cursor, m.offset)
	}
	m, _ = press(m, "home")
	if m.cursor != 0 || m.offset != 0 {
		t.Errorf("at home cursor=%d offset=%d, want 0 and 0", m.cursor, m.offset)
	}
}

func TestHeader(t *testing.T) {
	for _, tc := range []struct {
		watches []watch.Watch
		want    string
	}{
		{nil, "waitbuild: no builds"},
		{[]watch.Watch{running("a", now), running("b", now), finished("c", true, now)}, "waitbuild ⏳ 2 running"},
		{[]watch.Watch{finished("a", false, now), finished("b", true, now)}, "waitbuild ❌ last build failure"},
	} {
		if got := header(tc.watches); got != tc.want {
			t.Errorf("header = %q, want %q", got, tc.want)
		}
	}
}

func shas(watches []watch.Watch) []string {
	var out []string
	for _, w := range watches {
		out = append(out, w.SHA)
	}
	return out
}
