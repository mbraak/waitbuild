package watch

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveLoadRemove(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "watches") // created by Save
	t0 := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	older := Watch{PID: 1, Owner: "o", Repo: "r", Branch: "main", SHA: "aaaaaaaaaa", Started: t0}
	newer := Watch{PID: 2, Owner: "o", Repo: "r", Branch: "feature", SHA: "bbbbbbbbbb", Started: t0.Add(time.Minute),
		Checks: []Check{{Name: "build", Status: "in_progress"}}}

	if got, err := Load(dir); err != nil || got != nil {
		t.Fatalf("Load(missing dir) = %v, %v; want nil, nil", got, err)
	}
	for _, w := range []Watch{older, newer} {
		if err := Save(dir, w); err != nil {
			t.Fatal(err)
		}
	}
	// Junk that Load must skip: a leftover temp file, an unparsable file and
	// a file of another type.
	for name, content := range map[string]string{".tmp-123": "{}", "broken.json": "{", "notes.txt": "hi"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].SHA != newer.SHA || got[1].SHA != older.SHA {
		t.Fatalf("Load = %+v, want newer then older", got)
	}
	if len(got[0].Checks) != 1 || got[0].Checks[0].Name != "build" {
		t.Errorf("checks = %+v, want build", got[0].Checks)
	}

	// Saving again replaces the file instead of adding one.
	newer.Finished, newer.OK = true, true
	if err := Save(dir, newer); err != nil {
		t.Fatal(err)
	}
	if got, _ := Load(dir); len(got) != 2 || !got[0].Finished {
		t.Fatalf("after resave Load = %+v, want 2 watches, newer finished", got)
	}

	if err := Remove(dir, newer); err != nil {
		t.Fatal(err)
	}
	if err := Remove(dir, newer); err != nil {
		t.Errorf("removing twice: %v", err)
	}
	if got, _ := Load(dir); len(got) != 1 || got[0].SHA != older.SHA {
		t.Fatalf("after Remove Load = %+v, want only older", got)
	}
}

func TestState(t *testing.T) {
	orig := processAlive
	t.Cleanup(func() { processAlive = orig })
	processAlive = func(pid int) bool { return pid == 42 }

	tests := []struct {
		name string
		w    Watch
		want State
	}{
		{"polling", Watch{PID: 42}, Running},
		{"process gone", Watch{PID: 7}, Stopped},
		{"succeeded", Watch{PID: 7, Finished: true, OK: true}, Success},
		{"failed", Watch{PID: 42, Finished: true}, Failure},
		{"gave up", Watch{PID: 7, Finished: true, Error: "timeout"}, Error},
		{"no checks", Watch{PID: 7, Finished: true, CreatePullRequestURL: "u"}, NoChecks},
		{"cancelled", Watch{PID: 7, Finished: true, Checks: []Check{
			{Status: "completed", Conclusion: "success"},
			{Status: "completed", Conclusion: "cancelled"},
		}}, Cancelled},
		{"failed and cancelled", Watch{PID: 7, Finished: true, Checks: []Check{
			{Status: "completed", Conclusion: "failure"},
			{Status: "completed", Conclusion: "cancelled"},
		}}, Failure},
	}
	for _, tt := range tests {
		if got := tt.w.State(); got != tt.want {
			t.Errorf("%s: State() = %s, want %s", tt.name, got, tt.want)
		}
	}
}

func TestProcessAlive(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Error("own process reported as not running")
	}
	if processAlive(0) || processAlive(-1) {
		t.Error("invalid PID reported as running")
	}
}

// startSleep starts a process that runs until it is stopped, and reaps it
// when it exits so that it does not linger as a zombie.
func startSleep(t *testing.T) (pid int, exited <-chan struct{}) {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
	return cmd.Process.Pid, done
}

func TestDismissStopsWaitbuild(t *testing.T) {
	orig := processName
	t.Cleanup(func() { processName = orig })
	processName = func(int) string { return "waitbuild" }

	dir := t.TempDir()
	pid, exited := startSleep(t)
	w := Watch{PID: pid, Owner: "o", Repo: "r", SHA: "aaaaaaaaaa"}
	if err := Save(dir, w); err != nil {
		t.Fatal(err)
	}

	if err := Dismiss(dir, w); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	default:
		t.Error("the process is still running")
	}
	if got, _ := Load(dir); len(got) != 0 {
		t.Errorf("after Dismiss Load = %+v, want nothing", got)
	}
}

func TestDismissLeavesOtherProcesses(t *testing.T) {
	dir := t.TempDir()
	pid, exited := startSleep(t) // its pid could have been reused by sleep
	w := Watch{PID: pid, Owner: "o", Repo: "r", SHA: "aaaaaaaaaa"}
	if err := Save(dir, w); err != nil {
		t.Fatal(err)
	}

	if err := Dismiss(dir, w); err == nil {
		t.Error("Dismiss of a process that is not waitbuild succeeded")
	}
	select {
	case <-exited:
		t.Error("the process was stopped")
	default:
	}
	if got, _ := Load(dir); len(got) != 1 {
		t.Errorf("after a failed Dismiss Load = %+v, want the watch", got)
	}
}

func TestDismissFinished(t *testing.T) {
	dir := t.TempDir()
	w := Watch{PID: os.Getpid(), Owner: "o", Repo: "r", SHA: "aaaaaaaaaa", Finished: true}
	if err := Save(dir, w); err != nil {
		t.Fatal(err)
	}
	// The process is alive, but the build is finished: it is not stopped.
	if err := Dismiss(dir, w); err != nil {
		t.Fatal(err)
	}
	if got, _ := Load(dir); len(got) != 0 {
		t.Errorf("after Dismiss Load = %+v, want nothing", got)
	}
}

func TestDir(t *testing.T) {
	t.Setenv("WAITBUILD_STATE_DIR", "/some/dir")
	if got, err := Dir(); err != nil || got != "/some/dir" {
		t.Errorf("Dir() = %q, %v; want /some/dir", got, err)
	}
}

func TestCheckOK(t *testing.T) {
	for conclusion, want := range map[string]bool{"success": true, "skipped": true, "neutral": true, "failure": false, "": false} {
		if got := (Check{Conclusion: conclusion}).OK(); got != want {
			t.Errorf("OK(%q) = %v, want %v", conclusion, got, want)
		}
	}
}
