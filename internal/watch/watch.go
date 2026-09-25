// Package watch records the builds that running waitbuild processes are
// waiting for, so that other programs (the waitbuild-menubar app) can show
// them without querying GitHub themselves.
//
// Every waitbuild process writes one JSON file per watched commit into Dir and
// rewrites it after each poll. The file is left behind when the build finishes
// so that the result stays visible; readers delete it when it is no longer of
// interest.
package watch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Check is one check of the watched commit, as waitbuild last saw it.
type Check struct {
	Name       string `json:"name"`
	Status     string `json:"status"`               // as reported by GitHub; "completed" once finished
	Conclusion string `json:"conclusion,omitempty"` // set once completed
	URL        string `json:"url,omitempty"`
}

// Done reports whether the check has finished.
func (c Check) Done() bool { return c.Status == "completed" }

// OK reports whether the check finished successfully (or was skipped).
func (c Check) OK() bool {
	switch c.Conclusion {
	case "success", "skipped", "neutral":
		return true
	}
	return false
}

// Watch is the state of one waitbuild process waiting for a commit's build.
type Watch struct {
	PID      int       `json:"pid"`
	Owner    string    `json:"owner"`
	Repo     string    `json:"repo"`
	Branch   string    `json:"branch"`
	SHA      string    `json:"sha"`
	URL      string    `json:"url"` // page to open: the checks page, or the pull request once finished
	Started  time.Time `json:"started"`
	Updated  time.Time `json:"updated"`
	Finished bool      `json:"finished"`
	OK       bool      `json:"ok"`              // every check succeeded; only meaningful when Finished
	Error    string    `json:"error,omitempty"` // why waitbuild gave up, if it did
	Checks   []Check   `json:"checks"`
}

// State summarizes a watch for display.
type State string

const (
	Running State = "running" // waitbuild is still polling
	Success State = "success" // every check succeeded
	Failure State = "failure" // at least one check failed
	Error   State = "error"   // waitbuild gave up (timeout, no checks, API error)
	Stopped State = "stopped" // waitbuild exited without recording a result
)

// State returns the display state of w. An unfinished watch whose process no
// longer exists was interrupted (killed, machine went to sleep and rebooted).
func (w Watch) State() State {
	switch {
	case !w.Finished && !processAlive(w.PID):
		return Stopped
	case !w.Finished:
		return Running
	case w.Error != "":
		return Error
	case w.OK:
		return Success
	default:
		return Failure
	}
}

// ShortSHA returns the first 7 characters of the commit hash.
func (w Watch) ShortSHA() string { return w.SHA[:min(7, len(w.SHA))] }

// processAlive is a variable so tests can fake running processes.
var processAlive = func(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 only checks that the process exists. EPERM means it exists
	// but belongs to another user.
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Dir returns the directory holding the watch files: $WAITBUILD_STATE_DIR
// when set, otherwise "waitbuild/watches" in the user's cache directory.
func Dir() (string, error) {
	if d := os.Getenv("WAITBUILD_STATE_DIR"); d != "" {
		return d, nil
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "waitbuild", "watches"), nil
}

// fileName returns the name of the file for w. Two processes watching the
// same commit share a file; they report the same state anyway.
func (w Watch) fileName() string {
	return fmt.Sprintf("%s_%s_%s.json", w.Owner, w.Repo, w.SHA)
}

// Save writes w to dir, replacing the file atomically so that readers never
// see a partial write.
func Save(dir string, w Watch) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, w.fileName())); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// Load reads every watch in dir, most recently started first. A missing
// directory means there are no watches. Files that cannot be parsed are
// skipped.
func Load(dir string) ([]Watch, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var watches []Watch
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var w Watch
		if json.Unmarshal(data, &w) != nil || w.SHA == "" {
			continue
		}
		watches = append(watches, w)
	}
	sort.SliceStable(watches, func(i, j int) bool { return watches[i].Started.After(watches[j].Started) })
	return watches, nil
}

// Remove deletes the file of w from dir. A file that is already gone is not
// an error.
func Remove(dir string, w Watch) error {
	err := os.Remove(filepath.Join(dir, w.fileName()))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
