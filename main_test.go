package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/google/go-github/v90/github"
)

// --- parseGitHubRemote -----------------------------------------------------

func TestParseGitHubRemote(t *testing.T) {
	tests := []struct {
		url         string
		owner, repo string
		wantErr     bool
	}{
		{url: "git@github.com:mbraak/waitbuild.git", owner: "mbraak", repo: "waitbuild"},
		{url: "git@github.com:mbraak/waitbuild", owner: "mbraak", repo: "waitbuild"},
		{url: "ssh://git@github.com/mbraak/waitbuild.git", owner: "mbraak", repo: "waitbuild"},
		{url: "https://github.com/mbraak/waitbuild.git", owner: "mbraak", repo: "waitbuild"},
		{url: "https://github.com/mbraak/waitbuild", owner: "mbraak", repo: "waitbuild"},
		{url: "https://github.com/mbraak/waitbuild/", owner: "mbraak", repo: "waitbuild"},
		{url: "  https://github.com/mbraak/waitbuild.git\n", owner: "mbraak", repo: "waitbuild"},
		{url: "https://user:token@github.com/Some-Org/my.repo.git", owner: "Some-Org", repo: "my.repo"},
		{url: "https://gitlab.com/mbraak/waitbuild.git", wantErr: true},
		{url: "https://github.com/mbraak", wantErr: true},
		{url: "", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.url, func(t *testing.T) {
			owner, repo, err := parseGitHubRemote(tc.url)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseGitHubRemote(%q) = %q/%q, want error", tc.url, owner, repo)
				}
				if !strings.Contains(err.Error(), "not a github.com remote") {
					t.Errorf("error %q should mention github.com", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGitHubRemote(%q): %v", tc.url, err)
			}
			if owner != tc.owner || repo != tc.repo {
				t.Errorf("parseGitHubRemote(%q) = %q/%q, want %q/%q", tc.url, owner, repo, tc.owner, tc.repo)
			}
		})
	}
}

// --- githubToken -----------------------------------------------------------

func TestGithubToken(t *testing.T) {
	// A PATH without gh so the fallback cannot accidentally succeed.
	t.Setenv("PATH", t.TempDir())

	t.Run("GITHUB_TOKEN wins", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "from-github-token")
		t.Setenv("GH_TOKEN", "from-gh-token")
		got, err := githubToken()
		if err != nil || got != "from-github-token" {
			t.Fatalf("githubToken() = %q, %v; want %q", got, err, "from-github-token")
		}
	})

	t.Run("GH_TOKEN fallback", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "")
		t.Setenv("GH_TOKEN", "from-gh-token")
		got, err := githubToken()
		if err != nil || got != "from-gh-token" {
			t.Fatalf("githubToken() = %q, %v; want %q", got, err, "from-gh-token")
		}
	})

	t.Run("no token and no gh", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "")
		t.Setenv("GH_TOKEN", "")
		_, err := githubToken()
		if err == nil {
			t.Fatal("githubToken() succeeded without a token or gh")
		}
		if !strings.Contains(err.Error(), "gh auth login") {
			t.Errorf("error %q should tell the user how to log in", err)
		}
	})

	t.Run("gh auth token fallback", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("fake gh script needs a POSIX shell")
		}
		bin := t.TempDir()
		script := "#!/bin/sh\nif [ \"$1 $2\" = \"auth token\" ]; then printf '  gho_fake\\n'; else exit 1; fi\n"
		if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin)
		t.Setenv("GITHUB_TOKEN", "")
		t.Setenv("GH_TOKEN", "")
		got, err := githubToken()
		if err != nil || got != "gho_fake" {
			t.Fatalf("githubToken() = %q, %v; want trimmed %q", got, err, "gho_fake")
		}
	})
}

// --- repoInfo --------------------------------------------------------------

// initRepo creates a git repository with one commit on "main" and, when
// remote is not empty, an "origin" remote pointing at it.
func initRepo(t *testing.T, remote string) (dir string, repo *gogit.Repository, sha string) {
	t.Helper()
	dir = t.TempDir()
	repo, err := gogit.PlainInitWithOptions(dir, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{DefaultBranch: plumbing.Main},
	})
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("README"); err != nil {
		t.Fatal(err)
	}
	h, err := wt.Commit("initial", &gogit.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if remote != "" {
		_, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{remote}})
		if err != nil {
			t.Fatal(err)
		}
	}
	return dir, repo, h.String()
}

func TestRepoInfo(t *testing.T) {
	const remote = "git@github.com:mbraak/waitbuild.git"

	t.Run("on a branch", func(t *testing.T) {
		dir, _, sha := initRepo(t, remote)
		t.Chdir(dir)
		info, err := repoInfo()
		if err != nil {
			t.Fatal(err)
		}
		want := gitInfo{sha: sha, branch: "main", remote: remote}
		if info != want {
			t.Errorf("repoInfo() = %+v, want %+v", info, want)
		}
	})

	t.Run("from a subdirectory", func(t *testing.T) {
		dir, _, sha := initRepo(t, remote)
		sub := filepath.Join(dir, "a", "b")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(sub)
		info, err := repoInfo()
		if err != nil {
			t.Fatal(err)
		}
		if info.sha != sha || info.branch != "main" {
			t.Errorf("repoInfo() = %+v, want sha %s on main", info, sha)
		}
	})

	t.Run("detached HEAD", func(t *testing.T) {
		dir, repo, sha := initRepo(t, remote)
		wt, err := repo.Worktree()
		if err != nil {
			t.Fatal(err)
		}
		if err := wt.Checkout(&gogit.CheckoutOptions{Hash: plumbing.NewHash(sha)}); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		info, err := repoInfo()
		if err != nil {
			t.Fatal(err)
		}
		if info.branch != "HEAD" || info.sha != sha {
			t.Errorf("repoInfo() = %+v, want branch HEAD at %s", info, sha)
		}
	})

	t.Run("no origin remote", func(t *testing.T) {
		dir, _, _ := initRepo(t, "")
		t.Chdir(dir)
		_, err := repoInfo()
		if err == nil || !strings.Contains(err.Error(), "remote origin") {
			t.Fatalf("repoInfo() error = %v, want remote origin error", err)
		}
	})

	t.Run("not a repository", func(t *testing.T) {
		t.Chdir(t.TempDir())
		_, err := repoInfo()
		if err == nil || !strings.Contains(err.Error(), "opening git repository") {
			t.Fatalf("repoInfo() error = %v, want opening error", err)
		}
	})
}

// --- fake GitHub API -------------------------------------------------------

func mkRun(id int64, name, status, conclusion string) *github.WorkflowRun {
	r := &github.WorkflowRun{
		ID:      github.Ptr(id),
		Name:    github.Ptr(name),
		Status:  github.Ptr(status),
		HTMLURL: github.Ptr(fmt.Sprintf("https://github.com/o/r/actions/runs/%d", id)),
	}
	if conclusion != "" {
		r.Conclusion = github.Ptr(conclusion)
	}
	return r
}

func done(id int64, name, conclusion string) *github.WorkflowRun {
	return mkRun(id, name, "completed", conclusion)
}

// fakeAPI serves GET /repos/{owner}/{repo}/actions/runs. Each request is
// answered with the next entry of polls; once exhausted the last entry is
// repeated. A nil handler entry answers with HTTP 500.
type fakeAPI struct {
	mu       sync.Mutex
	polls    [][]*github.WorkflowRun
	pulls    []*github.PullRequest // served for .../commits/<sha>/pulls
	requests []*http.Request
	srv      *httptest.Server
}

func newFakeAPI(t *testing.T, polls ...[]*github.WorkflowRun) *fakeAPI {
	t.Helper()
	f := &fakeAPI{polls: polls}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.HasSuffix(r.URL.Path, "/pulls") {
		f.requests = append(f.requests, r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.pulls)
		return
	}
	if !strings.HasSuffix(r.URL.Path, "/actions/runs") {
		http.NotFound(w, r)
		return
	}
	i := len(f.requests)
	f.requests = append(f.requests, r)
	if i >= len(f.polls) {
		i = len(f.polls) - 1
	}
	runs := f.polls[i]
	if runs == nil {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(github.WorkflowRuns{
		TotalCount:   github.Ptr(len(runs)),
		WorkflowRuns: runs,
	})
}

func (f *fakeAPI) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// client returns a go-github client that talks to the fake server.
func (f *fakeAPI) client(t *testing.T) *github.Client {
	t.Helper()
	base := f.srv.URL + "/"
	c, err := github.NewClient(github.WithURLs(&base, &base))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func names(runs []*github.WorkflowRun) []string {
	var out []string
	for _, r := range runs {
		out = append(out, r.GetName())
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- listRuns --------------------------------------------------------------

func TestListRuns(t *testing.T) {
	t.Run("filters by sha and sorts by name", func(t *testing.T) {
		f := newFakeAPI(t, []*github.WorkflowRun{
			done(2, "lint", "success"),
			done(1, "build", "success"),
			done(3, "Deploy", "success"),
		})
		runs, err := listRuns(context.Background(), f.client(t), "o", "r", "abc123")
		if err != nil {
			t.Fatal(err)
		}
		if got, want := names(runs), []string{"Deploy", "build", "lint"}; !equalStrings(got, want) {
			t.Errorf("names = %v, want %v", got, want)
		}
		req := f.requests[0]
		if req.URL.Path != "/repos/o/r/actions/runs" {
			t.Errorf("path = %s", req.URL.Path)
		}
		if got := req.URL.Query().Get("head_sha"); got != "abc123" {
			t.Errorf("head_sha = %q, want abc123", got)
		}
		if got := req.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("per_page = %q, want 100", got)
		}
	})

	t.Run("follows pagination", func(t *testing.T) {
		var srv *httptest.Server
		var pages []string
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			page := r.URL.Query().Get("page")
			pages = append(pages, page)
			var runs []*github.WorkflowRun
			switch page {
			case "", "1":
				runs = []*github.WorkflowRun{done(1, "b", "success")}
				w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/actions/runs?page=2&per_page=100>; rel="next", <%s/repos/o/r/actions/runs?page=2&per_page=100>; rel="last"`, srv.URL, srv.URL))
			case "2":
				runs = []*github.WorkflowRun{done(2, "a", "success")}
			default:
				t.Errorf("unexpected page %q", page)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(github.WorkflowRuns{WorkflowRuns: runs})
		}))
		defer srv.Close()
		base := srv.URL + "/"
		c, err := github.NewClient(github.WithURLs(&base, &base))
		if err != nil {
			t.Fatal(err)
		}
		runs, err := listRuns(context.Background(), c, "o", "r", "sha")
		if err != nil {
			t.Fatal(err)
		}
		if got, want := names(runs), []string{"a", "b"}; !equalStrings(got, want) {
			t.Errorf("names = %v, want %v", got, want)
		}
		if got, want := pages, []string{"", "2"}; !equalStrings(got, want) {
			t.Errorf("pages requested = %q, want %q", got, want)
		}
	})

	t.Run("api error", func(t *testing.T) {
		f := newFakeAPI(t, nil)
		_, err := listRuns(context.Background(), f.client(t), "o", "r", "sha")
		if err == nil || !strings.Contains(err.Error(), "listing workflow runs") {
			t.Fatalf("listRuns() error = %v, want listing error", err)
		}
	})
}

// --- waitForRuns -----------------------------------------------------------

func TestWaitForRuns(t *testing.T) {
	const interval = time.Millisecond
	const appear = time.Minute

	t.Run("waits for runs to appear and complete, settling twice", func(t *testing.T) {
		f := newFakeAPI(t,
			nil, // placeholder replaced below: empty first poll
			[]*github.WorkflowRun{mkRun(1, "build", "in_progress", "")},
			[]*github.WorkflowRun{done(1, "build", "success"), mkRun(2, "lint", "queued", "")},
			[]*github.WorkflowRun{done(1, "build", "success"), done(2, "lint", "success")},
			[]*github.WorkflowRun{done(1, "build", "success"), done(2, "lint", "success")},
		)
		f.polls[0] = []*github.WorkflowRun{} // empty, not an error

		runs, err := waitForRuns(context.Background(), f.client(t), "o", "r", "sha", interval, appear)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := names(runs), []string{"build", "lint"}; !equalStrings(got, want) {
			t.Errorf("names = %v, want %v", got, want)
		}
		if n := f.requestCount(); n != 5 {
			t.Errorf("polled %d times, want 5 (empty, running, partial, done, done again)", n)
		}
	})

	t.Run("a late run resets the settle counter", func(t *testing.T) {
		f := newFakeAPI(t,
			[]*github.WorkflowRun{done(1, "build", "success")},
			[]*github.WorkflowRun{done(1, "build", "success"), mkRun(2, "lint", "in_progress", "")},
			[]*github.WorkflowRun{done(1, "build", "success"), done(2, "lint", "failure")},
			[]*github.WorkflowRun{done(1, "build", "success"), done(2, "lint", "failure")},
		)
		runs, err := waitForRuns(context.Background(), f.client(t), "o", "r", "sha", interval, appear)
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) != 2 {
			t.Fatalf("got %d runs, want 2 (late run must be included): %v", len(runs), names(runs))
		}
		if runs[1].GetConclusion() != "failure" {
			t.Errorf("lint conclusion = %q, want failure", runs[1].GetConclusion())
		}
		if n := f.requestCount(); n != 4 {
			t.Errorf("polled %d times, want 4", n)
		}
	})

	t.Run("gives up when no run appears", func(t *testing.T) {
		f := newFakeAPI(t, []*github.WorkflowRun{})
		_, err := waitForRuns(context.Background(), f.client(t), "o", "r", "deadbeef", interval, 20*time.Millisecond)
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "no workflow runs appeared for deadbeef") {
			t.Errorf("error = %q", err)
		}
		if n := f.requestCount(); n < 2 {
			t.Errorf("polled %d times, want at least 2 before giving up", n)
		}
	})

	t.Run("gives up when the context expires", func(t *testing.T) {
		f := newFakeAPI(t, []*github.WorkflowRun{mkRun(1, "build", "in_progress", "")})
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		_, err := waitForRuns(ctx, f.client(t), "o", "r", "sha", interval, appear)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want context.DeadlineExceeded", err)
		}
		// The deadline may hit while sleeping ("gave up waiting") or while
		// the HTTP request is in flight ("listing workflow runs").
		msg := err.Error()
		if !strings.Contains(msg, "gave up waiting") && !strings.Contains(msg, "listing workflow runs") {
			t.Errorf("error = %q", err)
		}
	})

	t.Run("propagates api errors", func(t *testing.T) {
		f := newFakeAPI(t, nil)
		_, err := waitForRuns(context.Background(), f.client(t), "o", "r", "sha", interval, appear)
		if err == nil || !strings.Contains(err.Error(), "listing workflow runs") {
			t.Fatalf("error = %v, want listing error", err)
		}
	})
}

// --- run (end to end against the fake API) ---------------------------------

// useFakeAPI points run() at f and checks the token is sent on every request.
func useFakeAPI(t *testing.T, f *fakeAPI, wantToken string) {
	t.Helper()
	orig := newGitHubClient
	t.Cleanup(func() { newGitHubClient = orig })
	newGitHubClient = func(token string) (*github.Client, error) {
		if token != wantToken {
			t.Errorf("client created with token %q, want %q", token, wantToken)
		}
		base := f.srv.URL + "/"
		return github.NewClient(github.WithURLs(&base, &base), github.WithAuthToken(token))
	}
}

func TestRun(t *testing.T) {
	const remote = "https://github.com/mbraak/waitbuild.git"
	const interval = time.Millisecond
	t.Setenv("PATH", t.TempDir()) // no gh, no osascript
	t.Setenv("GITHUB_TOKEN", "tok")
	t.Setenv("GH_TOKEN", "")

	t.Run("all runs succeed", func(t *testing.T) {
		dir, _, sha := initRepo(t, remote)
		t.Chdir(dir)
		ok := []*github.WorkflowRun{done(1, "build", "success"), done(2, "docs", "skipped"), done(3, "opt", "neutral")}
		f := newFakeAPI(t, ok)
		useFakeAPI(t, f, "tok")

		if err := run("", "", interval, time.Minute, time.Minute, true); err != nil {
			t.Fatalf("run() = %v, want nil", err)
		}
		req := f.requests[0]
		if req.URL.Path != "/repos/mbraak/waitbuild/actions/runs" {
			t.Errorf("path = %s, want repo from origin remote", req.URL.Path)
		}
		if got := req.URL.Query().Get("head_sha"); got != sha {
			t.Errorf("head_sha = %q, want HEAD %s", got, sha)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q, want Bearer tok", got)
		}
	})

	t.Run("a failed run makes run fail", func(t *testing.T) {
		dir, _, _ := initRepo(t, remote)
		t.Chdir(dir)
		f := newFakeAPI(t, []*github.WorkflowRun{done(1, "build", "success"), done(2, "test", "failure")})
		useFakeAPI(t, f, "tok")

		err := run("", "", interval, time.Minute, time.Minute, false)
		if err == nil || !strings.Contains(err.Error(), "did not succeed") {
			t.Fatalf("run() = %v, want failure", err)
		}
	})

	t.Run("cancelled run counts as failure", func(t *testing.T) {
		dir, _, _ := initRepo(t, remote)
		t.Chdir(dir)
		f := newFakeAPI(t, []*github.WorkflowRun{done(1, "build", "cancelled")})
		useFakeAPI(t, f, "tok")
		if err := run("", "", interval, time.Minute, time.Minute, false); err == nil {
			t.Fatal("run() = nil, want failure for cancelled run")
		}
	})

	t.Run("explicit sha overrides HEAD", func(t *testing.T) {
		dir, _, _ := initRepo(t, remote)
		t.Chdir(dir)
		f := newFakeAPI(t, []*github.WorkflowRun{done(1, "build", "success")})
		useFakeAPI(t, f, "tok")

		if err := run("0123456789abcdef", "feature", interval, time.Minute, time.Minute, false); err != nil {
			t.Fatal(err)
		}
		if got := f.requests[0].URL.Query().Get("head_sha"); got != "0123456789abcdef" {
			t.Errorf("head_sha = %q, want the -sha flag", got)
		}
	})

	t.Run("non-github remote", func(t *testing.T) {
		dir, _, _ := initRepo(t, "https://gitlab.com/mbraak/waitbuild.git")
		t.Chdir(dir)
		f := newFakeAPI(t, []*github.WorkflowRun{done(1, "build", "success")})
		useFakeAPI(t, f, "tok")

		err := run("", "", interval, time.Minute, time.Minute, false)
		if err == nil || !strings.Contains(err.Error(), "not a github.com remote") {
			t.Fatalf("run() = %v, want remote error", err)
		}
		if f.requestCount() != 0 {
			t.Error("run() should not call the API with a bad remote")
		}
	})

	t.Run("missing token", func(t *testing.T) {
		dir, _, _ := initRepo(t, remote)
		t.Chdir(dir)
		t.Setenv("GITHUB_TOKEN", "")
		f := newFakeAPI(t, []*github.WorkflowRun{done(1, "build", "success")})
		useFakeAPI(t, f, "")

		err := run("", "", interval, time.Minute, time.Minute, false)
		if err == nil || !strings.Contains(err.Error(), "no GitHub token") {
			t.Fatalf("run() = %v, want token error", err)
		}
	})

	t.Run("overall timeout", func(t *testing.T) {
		dir, _, _ := initRepo(t, remote)
		t.Chdir(dir)
		f := newFakeAPI(t, []*github.WorkflowRun{mkRun(1, "build", "in_progress", "")})
		useFakeAPI(t, f, "tok")

		err := run("", "", interval, time.Minute, 30*time.Millisecond, false)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("run() = %v, want deadline exceeded", err)
		}
	})

	t.Run("outside a repository", func(t *testing.T) {
		t.Chdir(t.TempDir())
		err := run("", "", interval, time.Minute, time.Minute, false)
		if err == nil || !strings.Contains(err.Error(), "opening git repository") {
			t.Fatalf("run() = %v, want repository error", err)
		}
	})
}

// --- desktopNotify ---------------------------------------------------------

func TestDesktopNotifyWithoutOsascript(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if got := desktopNotify("title", "message", "https://example.com"); got != "" {
		t.Fatalf("desktopNotify() = %q, want \"\" (silent no-op)", got)
	}
}

func TestDesktopNotifyUsesTerminalNotifier(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake terminal-notifier script needs a POSIX shell")
	}
	bin := t.TempDir()
	out := filepath.Join(bin, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + out + "\n"
	if err := os.WriteFile(filepath.Join(bin, "terminal-notifier"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	if used := desktopNotify("Build FAILED", "o/r @ abc", "https://github.com/o/r/actions/runs/1"); used != "terminal-notifier" {
		t.Fatalf("desktopNotify() = %q, want %q", used, "terminal-notifier")
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("terminal-notifier was not invoked: %v", err)
	}
	want := "-title\nBuild FAILED\n-message\no/r @ abc\n-group\nwaitbuild\n-open\nhttps://github.com/o/r/actions/runs/1\n"
	if string(got) != want {
		t.Fatalf("terminal-notifier args:\n%s\nwant:\n%s", got, want)
	}
}

// --- notifyURL -------------------------------------------------------------

func TestNotifyURL(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	checks := "https://github.com/o/r/commit/" + sha + "/checks"
	mk := func(conclusion, url string) *github.WorkflowRun {
		return &github.WorkflowRun{Conclusion: github.Ptr(conclusion), HTMLURL: github.Ptr(url)}
	}
	tests := []struct {
		name string
		runs []*github.WorkflowRun
		want string
	}{
		{"no runs", nil, checks},
		{"all succeeded", []*github.WorkflowRun{mk("success", "u1"), mk("skipped", "u2")}, checks},
		{"one failed", []*github.WorkflowRun{mk("success", "u1"), mk("failure", "u2")}, "u2"},
		{"one failed without url", []*github.WorkflowRun{mk("failure", "")}, checks},
		{"several failed", []*github.WorkflowRun{mk("failure", "u1"), mk("cancelled", "u2")}, checks},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := notifyURL("", "o", "r", sha, tt.runs); got != tt.want {
				t.Fatalf("notifyURL() = %q, want %q", got, tt.want)
			}
		})
	}
	t.Run("pull request wins over failed run", func(t *testing.T) {
		pr := "https://github.com/o/r/pull/7"
		if got := notifyURL(pr, "o", "r", sha, []*github.WorkflowRun{mk("failure", "u1")}); got != pr {
			t.Fatalf("notifyURL() = %q, want %q", got, pr)
		}
	})
}

// --- printPullRequest ------------------------------------------------------

// captureStdout runs fn and returns what it wrote to os.Stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()
	fn()
	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestPrintPullRequest(t *testing.T) {
	const remote = "https://github.com/mbraak/waitbuild.git"
	t.Setenv("PATH", t.TempDir()) // no gh
	t.Setenv("GITHUB_TOKEN", "tok")
	t.Setenv("GH_TOKEN", "")

	t.Run("prints the pull request url", func(t *testing.T) {
		dir, _, sha := initRepo(t, remote)
		t.Chdir(dir)
		f := newFakeAPI(t)
		f.pulls = []*github.PullRequest{{State: github.Ptr("open"), HTMLURL: github.Ptr("https://github.com/mbraak/waitbuild/pull/3")}}
		useFakeAPI(t, f, "tok")

		var err error
		out := captureStdout(t, func() { err = printPullRequest("") })
		if err != nil {
			t.Fatalf("printPullRequest() = %v, want nil", err)
		}
		if out != "https://github.com/mbraak/waitbuild/pull/3\n" {
			t.Fatalf("stdout = %q", out)
		}
		if want := "/repos/mbraak/waitbuild/commits/" + sha + "/pulls"; f.requests[0].URL.Path != want {
			t.Fatalf("request path = %q, want %q", f.requests[0].URL.Path, want)
		}
	})

	t.Run("fails when there is no pull request", func(t *testing.T) {
		dir, _, _ := initRepo(t, remote)
		t.Chdir(dir)
		f := newFakeAPI(t)
		useFakeAPI(t, f, "tok")

		var err error
		out := captureStdout(t, func() { err = printPullRequest("") })
		if err == nil || !strings.Contains(err.Error(), "no pull request found") {
			t.Fatalf("printPullRequest() = %v, want no-pull-request error", err)
		}
		if out != "" {
			t.Fatalf("stdout = %q, want empty", out)
		}
	})

	t.Run("fails outside a repository", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := printPullRequest(""); err == nil || !strings.Contains(err.Error(), "opening git repository") {
			t.Fatalf("printPullRequest() = %v, want repository error", err)
		}
	})
}

// --- pullRequestURL --------------------------------------------------------

func TestPullRequestURL(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	mk := func(state, url string) *github.PullRequest {
		return &github.PullRequest{State: github.Ptr(state), HTMLURL: github.Ptr(url)}
	}
	tests := []struct {
		name  string
		pulls []*github.PullRequest
		want  string
	}{
		{"no pull requests", nil, ""},
		{"one open", []*github.PullRequest{mk("open", "p1")}, "p1"},
		{"prefers open over closed", []*github.PullRequest{mk("closed", "p1"), mk("open", "p2")}, "p2"},
		{"only closed", []*github.PullRequest{mk("closed", "p1"), mk("closed", "p2")}, "p1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeAPI(t)
			f.pulls = tt.pulls
			if got := pullRequestURL(context.Background(), f.client(t), "o", "r", sha); got != tt.want {
				t.Fatalf("pullRequestURL() = %q, want %q", got, tt.want)
			}
		})
	}
	t.Run("lookup failure yields empty string", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)
		base := srv.URL + "/"
		c, err := github.NewClient(github.WithURLs(&base, &base))
		if err != nil {
			t.Fatal(err)
		}
		if got := pullRequestURL(context.Background(), c, "o", "r", sha); got != "" {
			t.Fatalf("pullRequestURL() = %q, want \"\"", got)
		}
	})
}
