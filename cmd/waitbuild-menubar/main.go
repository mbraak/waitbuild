// waitbuild-menubar shows the builds that waitbuild is waiting for in the
// macOS menu bar.
//
// Every waitbuild process records its build in a watch file (see package
// watch); this app only reads those files, so it makes no GitHub API calls of
// its own. The menu bar shows ⏳ with the number of running builds, or the
// result of the most recent build. Each build has a submenu with its checks;
// clicking a check opens it on GitHub.
//
// Finished builds stay in the menu until they are dismissed, cleared or older
// than -keep.
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

// render replaces the whole menu. Rebuilding is simpler than patching items
// in place, and happens at most once a minute unless a build changes.
// ResetMenu closes the ClickedCh of the removed items, which ends their
// goroutines.
func (a *app) render(m menu) {
	systray.ResetMenu()
	systray.SetTitle(m.title)
	systray.SetTooltip(m.tooltip)

	if len(m.entries) == 0 {
		systray.AddMenuItem("No builds", "").Disable()
	}
	for _, e := range m.entries {
		parent := systray.AddMenuItem(e.title, "")
		for _, it := range e.items {
			sub := parent.AddSubMenuItem(it.title, it.url)
			switch {
			case it.url != "":
				onClick(sub, func() { openURL(it.url) })
			case it.dismiss != nil:
				w := *it.dismiss
				onClick(sub, func() {
					_ = watch.Remove(a.dir, w)
					a.requestRefresh()
				})
			default:
				sub.Disable()
			}
		}
	}

	systray.AddSeparator()
	clear := systray.AddMenuItem("Clear finished", "Remove every build that is no longer running")
	if m.hasFinished {
		onClick(clear, a.clearFinished)
	} else {
		clear.Disable()
	}
	quit := systray.AddMenuItem("Quit", "")
	onClick(quit, systray.Quit)
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

// onClick calls f for every click on item until the item is removed.
func onClick(item *systray.MenuItem, f func()) {
	go func() {
		for range item.ClickedCh {
			f()
		}
	}()
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
