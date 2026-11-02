// waitbuild-menubar shows the builds that waitbuild is waiting for in the
// macOS menu bar.
//
// Every waitbuild process records its build in a watch file (see package
// watch); this app only reads those files, so it makes no GitHub API calls of
// its own. The menu bar shows ⏳ with the number of running builds, or the
// result of the most recent build. Each build has a submenu with its checks;
// clicking a check opens it on GitHub. A build whose commit is in no pull
// request has an item that opens GitHub's page for creating one.
//
// Finished builds stay in the menu until they are dismissed, cleared or older
// than -keep. A running build can be dismissed too, which stops its waitbuild
// process.
//
// A build that failed may be rerun on GitHub. To notice that, the app starts
// `waitbuild -if-rerun` for every build that did not succeed once per
// -rerun-interval (see rerunWatcher); the app itself still makes no GitHub
// API calls.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"sync"
	"time"

	"fyne.io/systray"
	"github.com/mbraak/waitbuild/internal/watch"
)

func main() {
	interval := flag.Duration("interval", 2*time.Second, "how often to reread the watch files")
	keep := flag.Duration("keep", 24*time.Hour, "how long finished builds stay in the menu")
	rerunInterval := flag.Duration("rerun-interval", time.Minute, "how often to check builds that did not succeed for a rerun on GitHub (0 disables)")
	waitbuildPath := flag.String("waitbuild", "", "path of the waitbuild binary used to check for reruns (default: next to this binary, else on PATH)")
	flag.Parse()

	dir, err := watch.Dir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "waitbuild-menubar:", err)
		os.Exit(1)
	}
	a := &app{dir: dir, keep: *keep, refresh: make(chan struct{}, 1)}
	if *rerunInterval > 0 {
		bin := *waitbuildPath
		if bin == "" {
			bin, err = findWaitbuild()
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "waitbuild-menubar: not checking for reruns, waitbuild not found:", err)
		} else {
			a.reruns = newRerunWatcher(bin, *rerunInterval)
		}
	}
	// systray.Run has to run on the main goroutine; the refresh loop runs in
	// its own goroutine and is the only one that changes the menu.
	systray.Run(func() { go a.loop(*interval) }, nil)
}

type app struct {
	dir     string
	keep    time.Duration
	refresh chan struct{} // requests an immediate refresh, after a click changed the files
	shown   *menu
	tray    *tray         // nil until the first render
	reruns  *rerunWatcher // nil when rerun checks are disabled
}

func (a *app) loop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		a.update()
		select {
		case <-ticker.C:
		case <-a.refresh:
		}
	}
}

// requestRefresh asks the loop to refresh now, without blocking.
func (a *app) requestRefresh() {
	select {
	case a.refresh <- struct{}{}:
	default:
	}
}

// update rereads the watch files, deletes those of builds that finished
// longer than keep ago and redraws the menu if anything changed.
func (a *app) update() {
	watches, err := watch.Load(a.dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "waitbuild-menubar:", err)
	}
	now := time.Now()
	kept := watches[:0]
	for _, w := range watches {
		if w.State() != watch.Running && now.Sub(w.Updated) > a.keep {
			_ = watch.Remove(a.dir, w)
			continue
		}
		kept = append(kept, w)
	}
	if a.reruns != nil {
		a.reruns.check(kept, now, a.requestRefresh)
	}

	m := buildMenu(kept, now)
	if a.shown != nil && reflect.DeepEqual(*a.shown, m) {
		return
	}
	a.shown = &m
	a.render(m)
}

// tray holds the menu items between renders. render updates them in place
// instead of rebuilding the menu, because removing the items of an open menu
// closes it. The top-level items are created up front, since systray can only
// append items and the entries have to stay above "Clear finished" and "Quit".
type tray struct {
	noBuilds *systray.MenuItem
	entries  [maxEntries]*traySlot
	clear    *actionItem
}

// traySlot is the top-level item at one position in the menu with its
// submenu. It grows when an entry needs more submenu items than it has and
// hides the ones it does not use.
type traySlot struct {
	item  *systray.MenuItem
	items []*actionItem
}

// actionItem is a menu item whose click action can change between renders.
type actionItem struct {
	*systray.MenuItem
	mu     sync.Mutex
	action func() // nil disables the item
}

func newActionItem(item *systray.MenuItem) *actionItem {
	a := &actionItem{MenuItem: item}
	go func() {
		for range item.ClickedCh {
			a.mu.Lock()
			f := a.action
			a.mu.Unlock()
			if f != nil {
				f()
			}
		}
	}()
	return a
}

func (a *actionItem) set(title, tooltip string, action func()) {
	a.mu.Lock()
	a.action = action
	a.mu.Unlock()
	a.SetTitle(title)
	a.SetTooltip(tooltip)
	if action == nil {
		a.Disable()
	} else {
		a.Enable()
	}
	a.Show()
}

func newTray() *tray {
	t := &tray{noBuilds: systray.AddMenuItem("No builds", "")}
	t.noBuilds.Disable()
	for i := range t.entries {
		item := systray.AddMenuItem("", "")
		item.Hide()
		t.entries[i] = &traySlot{item: item}
	}
	systray.AddSeparator()
	t.clear = newActionItem(systray.AddMenuItem("Clear finished", ""))
	quit := newActionItem(systray.AddMenuItem("Quit", ""))
	quit.action = systray.Quit
	return t
}

// render shows m by updating the existing menu items, so that an open menu
// stays open.
func (a *app) render(m menu) {
	if a.tray == nil {
		a.tray = newTray()
	}
	t := a.tray
	systray.SetTitle(m.title)
	systray.SetTooltip(m.tooltip)

	if len(m.entries) == 0 {
		t.noBuilds.Show()
	} else {
		t.noBuilds.Hide()
	}
	for i, s := range t.entries {
		if i >= len(m.entries) {
			s.item.Hide()
			continue
		}
		e := m.entries[i]
		s.item.SetTitle(e.title)
		s.item.Show()
		for j, it := range e.items {
			if j == len(s.items) {
				s.items = append(s.items, newActionItem(s.item.AddSubMenuItem("", "")))
			}
			s.items[j].set(it.title, it.url, a.action(it))
		}
		for _, sub := range s.items[len(e.items):] {
			sub.Hide()
		}
	}

	var clear func()
	if m.hasFinished {
		clear = a.clearFinished
	}
	t.clear.set("Clear finished", "Remove every build that is no longer running", clear)
}

// action returns what clicking it does, or nil when it is not clickable.
func (a *app) action(it item) func() {
	switch {
	case it.url != "":
		return func() { openURL(it.url) }
	case it.dismiss != nil:
		w := *it.dismiss
		return func() {
			if err := watch.Dismiss(a.dir, w); err != nil {
				fmt.Fprintln(os.Stderr, "waitbuild-menubar:", err)
			}
			a.requestRefresh()
		}
	default:
		return nil
	}
}

// clearFinished deletes the watch files of every build that is not running.
func (a *app) clearFinished() {
	watches, _ := watch.Load(a.dir)
	for _, w := range watches {
		if w.State() != watch.Running {
			_ = watch.Remove(a.dir, w)
		}
	}
	a.requestRefresh()
}

func openURL(url string) {
	cmd := "xdg-open"
	if runtime.GOOS == "darwin" {
		cmd = "open"
	}
	if err := exec.Command(cmd, url).Start(); err != nil {
		fmt.Fprintln(os.Stderr, "waitbuild-menubar: opening", url+":", err)
	}
}
