// waitbuild-tui shows the builds that waitbuild is waiting for in the
// terminal, like waitbuild-menubar does in the macOS menu bar.
//
// Every waitbuild process records its build in a watch file (see package
// watch); this program only reads those files, so it makes no GitHub API calls
// of its own. Each build can be expanded to show its checks; a check or build
// can be opened on GitHub.
//
// Finished builds stay listed until they are dismissed, cleared or older than
// -keep. Unlike waitbuild-menubar, it does not check failed builds for reruns
// on GitHub.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/mbraak/waitbuild/internal/watch"
)

func main() {
	interval := flag.Duration("interval", 2*time.Second, "how often to reread the watch files")
	keep := flag.Duration("keep", 24*time.Hour, "how long finished builds stay listed")
	flag.Parse()

	dir, err := watch.Dir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "waitbuild-tui:", err)
		os.Exit(1)
	}
	m := newModel(dir, *keep, *interval)
	if _, err := tea.NewProgram(m).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "waitbuild-tui:", err)
		os.Exit(1)
	}
}

func openURL(url string) error {
	cmd := "xdg-open"
	if runtime.GOOS == "darwin" {
		cmd = "open"
	}
	return exec.Command(cmd, url).Start()
}
