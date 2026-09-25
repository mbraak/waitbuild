package main

import (
	"fmt"
	"time"

	"github.com/mbraak/waitbuild/internal/watch"
)

// maxEntries caps the number of builds in the menu; older ones are left out.
const maxEntries = 15

// menu is what the menu bar shows, independent of the systray library so that
// it can be tested and compared between refreshes.
type menu struct {
	title       string // shown in the menu bar
	tooltip     string
	entries     []entry
	hasFinished bool // enables "Clear finished"
}

// entry is one watched build: a top-level item with a submenu.
type entry struct {
	title string
	items []item
}

// item is a submenu entry. It opens url when set, dismisses the watch when
// dismiss is set, and is shown disabled otherwise.
type item struct {
	title   string
	url     string
	dismiss *watch.Watch
}

var stateIcons = map[watch.State]string{
	watch.Running: "⏳",
	watch.Success: "✅",
	watch.Failure: "❌",
	watch.Error:   "⚠️",
	watch.Stopped: "⏹",
}

// idleTitle is shown in the menu bar when no builds are recorded.
const idleTitle = "○"

// buildMenu turns the recorded watches (most recent first) into a menu.
// The menu bar title counts the running builds, or shows the result of the
// most recent build when none is running.
func buildMenu(watches []watch.Watch, now time.Time) menu {
	m := menu{title: idleTitle, tooltip: "waitbuild: no builds"}
	running := 0
	for _, w := range watches {
		if w.State() == watch.Running {
			running++
		} else {
			m.hasFinished = true
		}
	}
	switch {
	case running > 0:
		m.title = fmt.Sprintf("%s %d", stateIcons[watch.Running], running)
		m.tooltip = fmt.Sprintf("waitbuild: %d running", running)
	case len(watches) > 0:
		m.title = stateIcons[watches[0].State()]
		m.tooltip = fmt.Sprintf("waitbuild: last build %s", watches[0].State())
	}

	for i, w := range watches {
		if i == maxEntries {
			break
		}
		m.entries = append(m.entries, buildEntry(w, now))
	}
	return m
}

func buildEntry(w watch.Watch, now time.Time) entry {
	state := w.State()
	title := fmt.Sprintf("%s %s/%s · %s · %s", stateIcons[state], w.Owner, w.Repo, w.Branch, w.ShortSHA())
	switch state {
	case watch.Running:
		done := 0
		for _, c := range w.Checks {
			if c.Done() {
				done++
			}
		}
		elapsed := "just started"
		if d := short(now.Sub(w.Started)); d != "" {
			elapsed = "running " + d
		}
		title += fmt.Sprintf(" — %d/%d, %s", done, len(w.Checks), elapsed)
	default:
		finished := "just now"
		if d := short(now.Sub(w.Updated)); d != "" {
			finished = d + " ago"
		}
		title += " — " + finished
	}

	e := entry{title: title}
	e.items = append(e.items, item{title: "Open on GitHub", url: w.URL})
	switch state {
	case watch.Error:
		e.items = append(e.items, item{title: "Gave up: " + w.Error})
	case watch.Stopped:
		e.items = append(e.items, item{title: "waitbuild exited before the build finished"})
	}
	if len(w.Checks) == 0 && state == watch.Running {
		e.items = append(e.items, item{title: "Waiting for checks to appear…"})
	}
	for _, c := range w.Checks {
		e.items = append(e.items, item{title: checkTitle(c), url: c.URL})
	}
	if state != watch.Running {
		e.items = append(e.items, item{title: "Dismiss", dismiss: &w})
	}
	return e
}

func checkTitle(c watch.Check) string {
	switch {
	case !c.Done():
		return fmt.Sprintf("⏳ %s — %s", c.Name, c.Status)
	case c.OK():
		return fmt.Sprintf("✔ %s — %s", c.Name, c.Conclusion)
	default:
		return fmt.Sprintf("✘ %s — %s", c.Name, c.Conclusion)
	}
}

// short formats d coarsely ("5m", "2h", "3d"), or returns "" when it is less
// than a minute. Minutes are the finest unit so that the menu only changes
// once a minute while nothing else happens.
func short(d time.Duration) string {
	switch {
	case d < time.Minute:
		return ""
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
}
