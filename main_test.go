package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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

// mkCheckRun builds a check run created by the GitHub App with slug app.
func mkCheckRun(id int64, name, app, status, conclusion string) *github.CheckRun {
	r := &github.CheckRun{
		ID:      github.Ptr(id),
		Name:    github.Ptr(name),
		Status:  github.Ptr(status),
		HTMLURL: github.Ptr(fmt.Sprintf("https://github.com/o/r/runs/%d", id)),
		App:     &github.App{Slug: github.Ptr(app)},
	}
	if conclusion != "" {
		r.Conclusion = github.Ptr(conclusion)
	}
	return r
}

// mkStatus builds a commit status such as CircleCI reports them.
func mkStatus(context, state string) *github.RepoStatus {
	return &github.RepoStatus{
		Context:   github.Ptr(context),
		State:     github.Ptr(state),
		TargetURL: github.Ptr("https://circleci.com/gh/o/r/" + strings.ReplaceAll(context, " ", "")),
	}
}

// poll is what the fake API reports for one polling round: the workflow runs,
// check runs and commit statuses of the commit.
type poll struct {
	runs      []*github.WorkflowRun
	checkRuns []*github.CheckRun
	statuses  []*github.RepoStatus
}

func runsOnly(runs ...*github.WorkflowRun) *poll { return &poll{runs: runs} }

// fakeAPI serves the endpoints waitbuild uses. The three per-commit listing
// endpoints (workflow runs, check runs, combined status) each answer their
// n-th request with polls[n]; once exhausted the last entry is repeated. A nil
// entry answers with HTTP 500.
type fakeAPI struct {
	mu          sync.Mutex
	polls       []*poll
	pulls       []*github.PullRequest // served for .../commits/<sha>/pulls
	noWorkflows bool                  // answer .../actions/workflows with an empty list
	requests    []*http.Request
	served      map[string]int // listing endpoint suffix -> requests answered
	srv         *httptest.Server
}

func newFakeAPI(t *testing.T, polls ...*poll) *fakeAPI {
	t.Helper()
	f := &fakeAPI{polls: polls, served: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r)
	w.Header().Set("Content-Type", "application/json")

	switch {
	case strings.HasSuffix(r.URL.Path, "/pulls"):
		_ = json.NewEncoder(w).Encode(f.pulls)
	case strings.HasSuffix(r.URL.Path, "/actions/workflows"):
		count := 1
		if f.noWorkflows {
			count = 0
		}
		_ = json.NewEncoder(w).Encode(github.Workflows{TotalCount: github.Ptr(count)})
	case strings.HasSuffix(r.URL.Path, "/actions/runs"):
		if p := f.poll("/actions/runs"); p == nil {
			http.Error(w, "boom", http.StatusInternalServerError)
		} else {
			_ = json.NewEncoder(w).Encode(github.WorkflowRuns{TotalCount: github.Ptr(len(p.runs)), WorkflowRuns: p.runs})
		}
	case strings.HasSuffix(r.URL.Path, "/check-runs"):
		if p := f.poll("/check-runs"); p == nil {
			http.Error(w, "boom", http.StatusInternalServerError)
		} else {
			_ = json.NewEncoder(w).Encode(github.ListCheckRunsResults{Total: github.Ptr(len(p.checkRuns)), CheckRuns: p.checkRuns})
		}
	case strings.HasSuffix(r.URL.Path, "/status"):
		if p := f.poll("/status"); p == nil {
			http.Error(w, "boom", http.StatusInternalServerError)
		} else {
			_ = json.NewEncoder(w).Encode(github.CombinedStatus{TotalCount: github.Ptr(len(p.statuses)), Statuses: p.statuses})
		}
	default:
		http.NotFound(w, r)
	}
}

// poll returns the entry for the next request to endpoint, or nil for an error.
func (f *fakeAPI) poll(endpoint string) *poll {
	i := f.served[endpoint]
	f.served[endpoint]++
	if len(f.polls) == 0 {
		return nil
	}
	if i >= len(f.polls) {
		i = len(f.polls) - 1
	}
	return f.polls[i]
}

// pollCount returns how many polling rounds were served.
func (f *fakeAPI) pollCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.served["/actions/runs"]
}

// request returns the first request whose path ends in suffix, or nil.
func (f *fakeAPI) request(suffix string) *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		if strings.HasSuffix(r.URL.Path, suffix) {
			return r
		}
	}
	return nil
}

// client returns a go-github client that talks to the fake server.
func (f *fakeAPI) client(t *testing.T) *github.Client {
	t.Helper()
	return clientFor(t, f.srv)
}

func clientFor(t *testing.T, srv *httptest.Server) *github.Client {
	t.Helper()
	base := srv.URL + "/"
	c, err := github.NewClient(github.WithURLs(&base, &base))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// pagedServer serves a two-page listing at any path: page 1 (also the
// unnumbered first request) with a Link header pointing at page 2. It
// records the page numbers requested.
func pagedServer(t *testing.T, page1, page2 any) (*httptest.Server, *[]string) {
	t.Helper()
	var srv *httptest.Server
	pages := &[]string{}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		*pages = append(*pages, page)
		w.Header().Set("Content-Type", "application/json")
		switch page {
		case "", "1":
			w.Header().Set("Link", fmt.Sprintf(`<%s%s?page=2&per_page=100>; rel="next", <%s%s?page=2&per_page=100>; rel="last"`, srv.URL, r.URL.Path, srv.URL, r.URL.Path))
			_ = json.NewEncoder(w).Encode(page1)
		case "2":
			_ = json.NewEncoder(w).Encode(page2)
		default:
			t.Errorf("unexpected page %q", page)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, pages
}

func names(checks []check) []string {
	var out []string
	for _, c := range checks {
		out = append(out, c.name)
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
	t.Run("filters by sha and maps runs to checks", func(t *testing.T) {
		f := newFakeAPI(t, runsOnly(done(2, "lint", "success"), mkRun(1, "build", "in_progress", "")))
		runs, err := listRuns(context.Background(), f.client(t), "o", "r", "abc123")
		if err != nil {
			t.Fatal(err)
		}
		want := []check{
			{key: "run:2", name: "lint", status: "completed", conclusion: "success", url: "https://github.com/o/r/actions/runs/2"},
			{key: "run:1", name: "build", status: "in_progress", url: "https://github.com/o/r/actions/runs/1"},
		}
		if !reflect.DeepEqual(runs, want) {
			t.Errorf("listRuns() = %+v, want %+v", runs, want)
		}
		req := f.request("/actions/runs")
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
		srv, pages := pagedServer(t,
			github.WorkflowRuns{WorkflowRuns: []*github.WorkflowRun{done(1, "b", "success")}},
			github.WorkflowRuns{WorkflowRuns: []*github.WorkflowRun{done(2, "a", "success")}},
		)
		runs, err := listRuns(context.Background(), clientFor(t, srv), "o", "r", "sha")
		if err != nil {
			t.Fatal(err)
		}
		if got, want := names(runs), []string{"b", "a"}; !equalStrings(got, want) {
			t.Errorf("names = %v, want %v", got, want)
		}
		if got, want := *pages, []string{"", "2"}; !equalStrings(got, want) {
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

// --- listCheckRuns ---------------------------------------------------------

func TestListCheckRuns(t *testing.T) {
	t.Run("maps check runs and skips GitHub Actions jobs", func(t *testing.T) {
		f := newFakeAPI(t, &poll{checkRuns: []*github.CheckRun{
			mkCheckRun(10, "SonarCloud Code Analysis", "sonarqubecloud", "completed", "success"),
			mkCheckRun(11, "danger", "github-actions", "completed", "success"),
			mkCheckRun(12, "Aikido Security: check code", "aikido-pr-checks", "in_progress", ""),
		}})
		got, err := listCheckRuns(context.Background(), f.client(t), "o", "r", "abc123")
		if err != nil {
			t.Fatal(err)
		}
		want := []check{
			{key: "check:10", name: "SonarCloud Code Analysis", status: "completed", conclusion: "success", url: "https://github.com/o/r/runs/10"},
			{key: "check:12", name: "Aikido Security: check code", status: "in_progress", url: "https://github.com/o/r/runs/12"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("listCheckRuns() = %+v, want %+v", got, want)
		}
		req := f.request("/check-runs")
		if req.URL.Path != "/repos/o/r/commits/abc123/check-runs" {
			t.Errorf("path = %s", req.URL.Path)
		}
		if got := req.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("per_page = %q, want 100", got)
		}
	})

	t.Run("follows pagination", func(t *testing.T) {
		srv, pages := pagedServer(t,
			github.ListCheckRunsResults{CheckRuns: []*github.CheckRun{mkCheckRun(1, "b", "sonar", "completed", "success")}},
			github.ListCheckRunsResults{CheckRuns: []*github.CheckRun{mkCheckRun(2, "a", "sonar", "completed", "success")}},
		)
		got, err := listCheckRuns(context.Background(), clientFor(t, srv), "o", "r", "sha")
		if err != nil {
			t.Fatal(err)
		}
		if got, want := names(got), []string{"b", "a"}; !equalStrings(got, want) {
			t.Errorf("names = %v, want %v", got, want)
		}
		if got, want := *pages, []string{"", "2"}; !equalStrings(got, want) {
			t.Errorf("pages requested = %q, want %q", got, want)
		}
	})

	t.Run("api error", func(t *testing.T) {
		f := newFakeAPI(t, nil)
		_, err := listCheckRuns(context.Background(), f.client(t), "o", "r", "sha")
		if err == nil || !strings.Contains(err.Error(), "listing check runs") {
			t.Fatalf("listCheckRuns() error = %v, want listing error", err)
		}
	})
}

// --- listStatuses ----------------------------------------------------------

func TestListStatuses(t *testing.T) {
	t.Run("maps commit statuses to checks keyed by context", func(t *testing.T) {
		f := newFakeAPI(t, &poll{statuses: []*github.RepoStatus{
			mkStatus("ci/circleci: lint", "success"),
			mkStatus("ci/circleci: test", "pending"),
			mkStatus("ci/circleci: build", "failure"),
			mkStatus("ci/circleci: deploy", "error"),
		}})
		got, err := listStatuses(context.Background(), f.client(t), "o", "r", "abc123")
		if err != nil {
			t.Fatal(err)
		}
		want := []check{
			{key: "status:ci/circleci: lint", name: "ci/circleci: lint", status: "completed", conclusion: "success", url: "https://circleci.com/gh/o/r/ci/circleci:lint"},
			{key: "status:ci/circleci: test", name: "ci/circleci: test", status: "pending", url: "https://circleci.com/gh/o/r/ci/circleci:test"},
			{key: "status:ci/circleci: build", name: "ci/circleci: build", status: "completed", conclusion: "failure", url: "https://circleci.com/gh/o/r/ci/circleci:build"},
			{key: "status:ci/circleci: deploy", name: "ci/circleci: deploy", status: "completed", conclusion: "error", url: "https://circleci.com/gh/o/r/ci/circleci:deploy"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("listStatuses() = %+v, want %+v", got, want)
		}
		for _, c := range got[2:] {
			if c.ok() {
				t.Errorf("%s with conclusion %q counts as ok, want failure", c.name, c.conclusion)
			}
		}
		req := f.request("/status")
		if req.URL.Path != "/repos/o/r/commits/abc123/status" {
			t.Errorf("path = %s", req.URL.Path)
		}
		if got := req.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("per_page = %q, want 100", got)
		}
	})

	t.Run("follows pagination", func(t *testing.T) {
		srv, pages := pagedServer(t,
			github.CombinedStatus{Statuses: []*github.RepoStatus{mkStatus("b", "success")}},
			github.CombinedStatus{Statuses: []*github.RepoStatus{mkStatus("a", "success")}},
		)
		got, err := listStatuses(context.Background(), clientFor(t, srv), "o", "r", "sha")
		if err != nil {
			t.Fatal(err)
		}
		if got, want := names(got), []string{"b", "a"}; !equalStrings(got, want) {
			t.Errorf("names = %v, want %v", got, want)
		}
		if got, want := *pages, []string{"", "2"}; !equalStrings(got, want) {
			t.Errorf("pages requested = %q, want %q", got, want)
		}
	})

	t.Run("api error", func(t *testing.T) {
		f := newFakeAPI(t, nil)
		_, err := listStatuses(context.Background(), f.client(t), "o", "r", "sha")
		if err == nil || !strings.Contains(err.Error(), "listing commit statuses") {
			t.Fatalf("listStatuses() error = %v, want listing error", err)
		}
	})
}

// --- listChecks ------------------------------------------------------------

func TestListChecks(t *testing.T) {
	t.Run("merges all sources sorted by name", func(t *testing.T) {
		f := newFakeAPI(t, &poll{
			runs:      []*github.WorkflowRun{done(1, "danger", "success")},
			checkRuns: []*github.CheckRun{mkCheckRun(2, "SonarCloud", "sonarqubecloud", "completed", "success"), mkCheckRun(3, "danger", "github-actions", "completed", "success")},
			statuses:  []*github.RepoStatus{mkStatus("ci/circleci: lint", "pending"), mkStatus("ci/circleci: build", "success")},
		})
		got, err := listChecks(context.Background(), f.client(t), "o", "r", "sha")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"SonarCloud", "ci/circleci: build", "ci/circleci: lint", "danger"}
		if !equalStrings(names(got), want) {
			t.Errorf("names = %v, want %v", names(got), want)
		}
	})

	t.Run("fails when any source fails", func(t *testing.T) {
		f := newFakeAPI(t, nil)
		_, err := listChecks(context.Background(), f.client(t), "o", "r", "sha")
		if err == nil {
			t.Fatal("listChecks() = nil, want error")
		}
	})
}

// --- waitForChecks ---------------------------------------------------------

func TestWaitForChecks(t *testing.T) {
	const interval = time.Millisecond
	const appear = time.Minute
	wait := func(f *fakeAPI, ctx context.Context, appear time.Duration) ([]check, error) {
		return waitForChecks(ctx, f.client(t), "o", "r", "sha", interval, appear, true)
	}

	t.Run("waits for runs to appear and complete, settling twice", func(t *testing.T) {
		f := newFakeAPI(t,
			&poll{}, // nothing reported yet
			runsOnly(mkRun(1, "build", "in_progress", "")),
			runsOnly(done(1, "build", "success"), mkRun(2, "lint", "queued", "")),
			runsOnly(done(1, "build", "success"), done(2, "lint", "success")),
			runsOnly(done(1, "build", "success"), done(2, "lint", "success")),
		)
		checks, err := wait(f, context.Background(), appear)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := names(checks), []string{"build", "lint"}; !equalStrings(got, want) {
			t.Errorf("names = %v, want %v", got, want)
		}
		if n := f.pollCount(); n != 5 {
			t.Errorf("polled %d times, want 5 (empty, running, partial, done, done again)", n)
		}
	})

	t.Run("a late run resets the settle counter", func(t *testing.T) {
		f := newFakeAPI(t,
			runsOnly(done(1, "build", "success")),
			runsOnly(done(1, "build", "success"), mkRun(2, "lint", "in_progress", "")),
			runsOnly(done(1, "build", "success"), done(2, "lint", "failure")),
			runsOnly(done(1, "build", "success"), done(2, "lint", "failure")),
		)
		checks, err := wait(f, context.Background(), appear)
		if err != nil {
			t.Fatal(err)
		}
		if len(checks) != 2 {
			t.Fatalf("got %d checks, want 2 (late run must be included): %v", len(checks), names(checks))
		}
		if checks[1].conclusion != "failure" {
			t.Errorf("lint conclusion = %q, want failure", checks[1].conclusion)
		}
		if n := f.pollCount(); n != 4 {
			t.Errorf("polled %d times, want 4", n)
		}
	})

	t.Run("keeps waiting while CircleCI statuses are pending", func(t *testing.T) {
		actionsDone := []*github.WorkflowRun{done(1, "danger", "success")}
		f := newFakeAPI(t,
			&poll{runs: actionsDone, statuses: []*github.RepoStatus{mkStatus("ci/circleci: lint", "pending"), mkStatus("ci/circleci: test", "pending")}},
			&poll{runs: actionsDone, statuses: []*github.RepoStatus{mkStatus("ci/circleci: lint", "success"), mkStatus("ci/circleci: test", "pending")}},
			&poll{runs: actionsDone, statuses: []*github.RepoStatus{mkStatus("ci/circleci: lint", "success"), mkStatus("ci/circleci: test", "failure")}},
			&poll{runs: actionsDone, statuses: []*github.RepoStatus{mkStatus("ci/circleci: lint", "success"), mkStatus("ci/circleci: test", "failure")}},
		)
		checks, err := wait(f, context.Background(), appear)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := names(checks), []string{"ci/circleci: lint", "ci/circleci: test", "danger"}; !equalStrings(got, want) {
			t.Errorf("names = %v, want %v", got, want)
		}
		if checks[1].conclusion != "failure" {
			t.Errorf("test conclusion = %q, want failure", checks[1].conclusion)
		}
		if n := f.pollCount(); n != 4 {
			t.Errorf("polled %d times, want 4", n)
		}
	})

	t.Run("a status registering after Actions finished resets the settle counter", func(t *testing.T) {
		actionsDone := []*github.WorkflowRun{done(1, "danger", "success")}
		f := newFakeAPI(t,
			&poll{runs: actionsDone},
			&poll{runs: actionsDone, statuses: []*github.RepoStatus{mkStatus("ci/circleci: lint", "pending")}},
			&poll{runs: actionsDone, statuses: []*github.RepoStatus{mkStatus("ci/circleci: lint", "success")}},
			&poll{runs: actionsDone, statuses: []*github.RepoStatus{mkStatus("ci/circleci: lint", "success")}},
		)
		checks, err := wait(f, context.Background(), appear)
		if err != nil {
			t.Fatal(err)
		}
		if len(checks) != 2 {
			t.Fatalf("got %d checks, want 2 (late status must be included): %v", len(checks), names(checks))
		}
		if n := f.pollCount(); n != 4 {
			t.Errorf("polled %d times, want 4", n)
		}
	})

	t.Run("a check run from another app is waited for", func(t *testing.T) {
		f := newFakeAPI(t,
			&poll{checkRuns: []*github.CheckRun{mkCheckRun(1, "SonarCloud", "sonarqubecloud", "in_progress", "")}},
			&poll{checkRuns: []*github.CheckRun{mkCheckRun(1, "SonarCloud", "sonarqubecloud", "completed", "success")}},
			&poll{checkRuns: []*github.CheckRun{mkCheckRun(1, "SonarCloud", "sonarqubecloud", "completed", "success")}},
		)
		checks, err := wait(f, context.Background(), appear)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := names(checks), []string{"SonarCloud"}; !equalStrings(got, want) {
			t.Errorf("names = %v, want %v", got, want)
		}
		if n := f.pollCount(); n != 3 {
			t.Errorf("polled %d times, want 3", n)
		}
	})

	t.Run("gives up when no check appears", func(t *testing.T) {
		f := newFakeAPI(t, &poll{})
		_, err := waitForChecks(context.Background(), f.client(t), "o", "r", "deadbeef", interval, 20*time.Millisecond, true)
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "no checks appeared for deadbeef") || !strings.Contains(err.Error(), "was the commit pushed?") {
			t.Errorf("error = %q", err)
		}
		if n := f.pollCount(); n < 2 {
			t.Errorf("polled %d times, want at least 2 before giving up", n)
		}
	})

	t.Run("mentions the missing workflows when nothing appears", func(t *testing.T) {
		f := newFakeAPI(t, &poll{})
		_, err := waitForChecks(context.Background(), f.client(t), "o", "r", "deadbeef", interval, 20*time.Millisecond, false)
		if err == nil || !strings.Contains(err.Error(), "no GitHub Actions workflows") {
			t.Fatalf("error = %v, want a hint about missing workflows", err)
		}
	})

	t.Run("gives up when the context expires", func(t *testing.T) {
		f := newFakeAPI(t, runsOnly(mkRun(1, "build", "in_progress", "")))
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		_, err := wait(f, ctx, appear)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want context.DeadlineExceeded", err)
		}
		// The deadline may hit while sleeping ("gave up waiting") or while
		// one of the HTTP requests is in flight ("listing ...").
		msg := err.Error()
		if !strings.Contains(msg, "gave up waiting") && !strings.Contains(msg, "listing ") {
			t.Errorf("error = %q", err)
		}
	})

	t.Run("propagates api errors", func(t *testing.T) {
		f := newFakeAPI(t, nil)
		_, err := wait(f, context.Background(), appear)
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

	t.Run("all checks succeed", func(t *testing.T) {
		dir, _, sha := initRepo(t, remote)
		t.Chdir(dir)
		f := newFakeAPI(t, &poll{
			runs:      []*github.WorkflowRun{done(1, "build", "success"), done(2, "docs", "skipped"), done(3, "opt", "neutral")},
			checkRuns: []*github.CheckRun{mkCheckRun(4, "SonarCloud", "sonarqubecloud", "completed", "success")},
			statuses:  []*github.RepoStatus{mkStatus("ci/circleci: lint", "success")},
		})
		useFakeAPI(t, f, "tok")

		out := captureStdout(t, func() {
			if err := run("", "", interval, time.Minute, time.Minute, true); err != nil {
				t.Errorf("run() = %v, want nil", err)
			}
		})
		for _, want := range []string{"✔ build", "✔ SonarCloud", "✔ ci/circleci: lint"} {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		}
		for _, suffix := range []string{"/actions/runs", "/check-runs", "/status"} {
			req := f.request(suffix)
			if req == nil {
				t.Fatalf("no request to %s", suffix)
			}
			if !strings.HasPrefix(req.URL.Path, "/repos/mbraak/waitbuild/") {
				t.Errorf("path = %s, want repo from origin remote", req.URL.Path)
			}
			if got := req.Header.Get("Authorization"); got != "Bearer tok" {
				t.Errorf("%s: Authorization = %q, want Bearer tok", suffix, got)
			}
		}
		if got := f.request("/actions/runs").URL.Query().Get("head_sha"); got != sha {
			t.Errorf("head_sha = %q, want HEAD %s", got, sha)
		}
		for _, suffix := range []string{"/check-runs", "/status"} {
			if want := "/repos/mbraak/waitbuild/commits/" + sha + suffix; f.request(suffix).URL.Path != want {
				t.Errorf("path = %s, want %s", f.request(suffix).URL.Path, want)
			}
		}
	})

	t.Run("repository without workflows waits for statuses", func(t *testing.T) {
		dir, _, _ := initRepo(t, remote)
		t.Chdir(dir)
		f := newFakeAPI(t,
			&poll{statuses: []*github.RepoStatus{mkStatus("ci/circleci: test", "pending")}},
			&poll{statuses: []*github.RepoStatus{mkStatus("ci/circleci: test", "success")}},
		)
		f.noWorkflows = true
		useFakeAPI(t, f, "tok")

		out := captureStdout(t, func() {
			if err := run("", "", interval, time.Minute, time.Minute, false); err != nil {
				t.Errorf("run() = %v, want nil", err)
			}
		})
		if !strings.Contains(out, "has no GitHub Actions workflows") {
			t.Errorf("output should mention the missing workflows:\n%s", out)
		}
		if n := f.pollCount(); n < 3 {
			t.Errorf("polled %d times, want at least 3", n)
		}
	})

	t.Run("repository without workflows and without checks gives up", func(t *testing.T) {
		dir, _, _ := initRepo(t, remote)
		t.Chdir(dir)
		f := newFakeAPI(t, &poll{})
		f.noWorkflows = true
		useFakeAPI(t, f, "tok")

		err := run("", "", interval, 20*time.Millisecond, time.Minute, false)
		if err == nil || !strings.Contains(err.Error(), "no checks appeared") || !strings.Contains(err.Error(), "no GitHub Actions workflows") {
			t.Fatalf("run() = %v, want no-checks error mentioning the missing workflows", err)
		}
	})

	t.Run("a failed run makes run fail", func(t *testing.T) {
		dir, _, _ := initRepo(t, remote)
		t.Chdir(dir)
		f := newFakeAPI(t, runsOnly(done(1, "build", "success"), done(2, "test", "failure")))
		useFakeAPI(t, f, "tok")

		err := run("", "", interval, time.Minute, time.Minute, false)
		if err == nil || !strings.Contains(err.Error(), "did not succeed") {
			t.Fatalf("run() = %v, want failure", err)
		}
	})

	t.Run("a failed CircleCI status makes run fail", func(t *testing.T) {
		dir, _, _ := initRepo(t, remote)
		t.Chdir(dir)
		f := newFakeAPI(t, &poll{
			runs:     []*github.WorkflowRun{done(1, "danger", "success")},
			statuses: []*github.RepoStatus{mkStatus("ci/circleci: lint", "success"), mkStatus("ci/circleci: test", "failure")},
		})
		useFakeAPI(t, f, "tok")

		var err error
		out := captureStdout(t, func() { err = run("", "", interval, time.Minute, time.Minute, false) })
		if err == nil || !strings.Contains(err.Error(), "did not succeed") {
			t.Fatalf("run() = %v, want failure", err)
		}
		if !strings.Contains(out, "✘ ci/circleci: test") {
			t.Errorf("output should mark the failed status:\n%s", out)
		}
	})

	t.Run("an errored status counts as failure", func(t *testing.T) {
		dir, _, _ := initRepo(t, remote)
		t.Chdir(dir)
		f := newFakeAPI(t, &poll{statuses: []*github.RepoStatus{mkStatus("ci/circleci: build", "error")}})
		useFakeAPI(t, f, "tok")
		if err := run("", "", interval, time.Minute, time.Minute, false); err == nil {
			t.Fatal("run() = nil, want failure for errored status")
		}
	})

	t.Run("a failed check run from another app makes run fail", func(t *testing.T) {
		dir, _, _ := initRepo(t, remote)
		t.Chdir(dir)
		f := newFakeAPI(t, &poll{
			runs:      []*github.WorkflowRun{done(1, "danger", "success")},
			checkRuns: []*github.CheckRun{mkCheckRun(2, "SonarCloud", "sonarqubecloud", "completed", "failure")},
		})
		useFakeAPI(t, f, "tok")
		if err := run("", "", interval, time.Minute, time.Minute, false); err == nil {
			t.Fatal("run() = nil, want failure for failed check run")
		}
	})

	t.Run("cancelled run counts as failure", func(t *testing.T) {
		dir, _, _ := initRepo(t, remote)
		t.Chdir(dir)
		f := newFakeAPI(t, runsOnly(done(1, "build", "cancelled")))
		useFakeAPI(t, f, "tok")
		if err := run("", "", interval, time.Minute, time.Minute, false); err == nil {
			t.Fatal("run() = nil, want failure for cancelled run")
		}
	})

	t.Run("explicit sha overrides HEAD", func(t *testing.T) {
		dir, _, _ := initRepo(t, remote)
		t.Chdir(dir)
		f := newFakeAPI(t, runsOnly(done(1, "build", "success")))
		useFakeAPI(t, f, "tok")

		if err := run("0123456789abcdef", "feature", interval, time.Minute, time.Minute, false); err != nil {
			t.Fatal(err)
		}
		if got := f.request("/actions/runs").URL.Query().Get("head_sha"); got != "0123456789abcdef" {
			t.Errorf("head_sha = %q, want the -sha flag", got)
		}
		if got := f.request("/status").URL.Path; got != "/repos/mbraak/waitbuild/commits/0123456789abcdef/status" {
			t.Errorf("status path = %q, want the -sha flag", got)
		}
	})

	t.Run("non-github remote", func(t *testing.T) {
		dir, _, _ := initRepo(t, "https://gitlab.com/mbraak/waitbuild.git")
		t.Chdir(dir)
		f := newFakeAPI(t, runsOnly(done(1, "build", "success")))
		useFakeAPI(t, f, "tok")

		err := run("", "", interval, time.Minute, time.Minute, false)
		if err == nil || !strings.Contains(err.Error(), "not a github.com remote") {
			t.Fatalf("run() = %v, want remote error", err)
		}
		if len(f.requests) != 0 {
			t.Error("run() should not call the API with a bad remote")
		}
	})

	t.Run("missing token", func(t *testing.T) {
		dir, _, _ := initRepo(t, remote)
		t.Chdir(dir)
		t.Setenv("GITHUB_TOKEN", "")
		f := newFakeAPI(t, runsOnly(done(1, "build", "success")))
		useFakeAPI(t, f, "")

		err := run("", "", interval, time.Minute, time.Minute, false)
		if err == nil || !strings.Contains(err.Error(), "no GitHub token") {
			t.Fatalf("run() = %v, want token error", err)
		}
	})

	t.Run("overall timeout", func(t *testing.T) {
		dir, _, _ := initRepo(t, remote)
		t.Chdir(dir)
		f := newFakeAPI(t, &poll{statuses: []*github.RepoStatus{mkStatus("ci/circleci: test", "pending")}})
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
	if got := desktopNotify("title", "message", "https://example.com", ""); got != "" {
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

	if used := desktopNotify("Build FAILED", "o/r @ abc", "https://github.com/o/r/actions/runs/1", "/icons/failure.png"); used != "terminal-notifier" {
		t.Fatalf("desktopNotify() = %q, want %q", used, "terminal-notifier")
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("terminal-notifier was not invoked: %v", err)
	}
	want := "-title\nBuild FAILED\n-message\no/r @ abc\n-group\nwaitbuild\n-open\nhttps://github.com/o/r/actions/runs/1\n-contentImage\n/icons/failure.png\n"
	if string(got) != want {
		t.Fatalf("terminal-notifier args:\n%s\nwant:\n%s", got, want)
	}
}

// --- iconFile / drawIcon ---------------------------------------------------

func TestIconFileWritesDistinctCachedIcons(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache) // linux
	t.Setenv("HOME", cache)           // darwin uses $HOME/Library/Caches
	t.Setenv("LocalAppData", cache)   // windows

	success, failure := iconFile(true), iconFile(false)
	if success == "" || failure == "" {
		t.Fatalf("iconFile() = %q, %q, want two paths", success, failure)
	}
	if success == failure {
		t.Fatalf("iconFile(true) and iconFile(false) both returned %q", success)
	}
	for _, p := range []string{success, failure} {
		if !strings.HasPrefix(p, cache) {
			t.Errorf("icon %q not under cache dir %q", p, cache)
		}
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := png.DecodeConfig(f)
		f.Close()
		if err != nil {
			t.Fatalf("%s: not a PNG: %v", p, err)
		}
		if cfg.Width != 128 || cfg.Height != 128 {
			t.Errorf("%s: size %dx%d, want 128x128", p, cfg.Width, cfg.Height)
		}
	}
	if again := iconFile(true); again != success {
		t.Fatalf("second iconFile(true) = %q, want cached %q", again, success)
	}
}

func TestDrawIconColors(t *testing.T) {
	ok, fail := drawIcon(true), drawIcon(false)
	// The filled disc shows the status colour off-centre, away from the mark.
	if c := ok.RGBAAt(20, 64); c.G < 0x90 || c.R > 0x60 {
		t.Errorf("success icon fill = %v, want green", c)
	}
	if c := fail.RGBAAt(20, 64); c.R < 0xa0 || c.G > 0x60 {
		t.Errorf("failure icon fill = %v, want red", c)
	}
	// The mark is white where both icons have a stroke.
	if c := fail.RGBAAt(64, 64); c.R != 0xff || c.G != 0xff || c.B != 0xff {
		t.Errorf("failure icon centre = %v, want white cross", c)
	}
	if c := ok.RGBAAt(56, 86); c.R != 0xff || c.G != 0xff || c.B != 0xff {
		t.Errorf("success icon check corner = %v, want white check mark", c)
	}
	// Corners lie outside the disc and are transparent.
	if c := ok.RGBAAt(0, 0); c.A != 0 {
		t.Errorf("corner = %v, want transparent", c)
	}
}

// --- notifyURL -------------------------------------------------------------

func TestNotifyURL(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	checks := "https://github.com/o/r/commit/" + sha + "/checks"
	mk := func(conclusion, url string) check {
		return check{status: "completed", conclusion: conclusion, url: url}
	}
	tests := []struct {
		name   string
		checks []check
		want   string
	}{
		{"no checks", nil, checks},
		{"all succeeded", []check{mk("success", "u1"), mk("skipped", "u2")}, checks},
		{"one failed", []check{mk("success", "u1"), mk("failure", "u2")}, "u2"},
		{"one failed status", []check{mk("success", "u1"), mk("error", "https://circleci.com/x")}, "https://circleci.com/x"},
		{"one failed without url", []check{mk("failure", "")}, checks},
		{"several failed", []check{mk("failure", "u1"), mk("cancelled", "u2")}, checks},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := notifyURL("", "o", "r", sha, tt.checks); got != tt.want {
				t.Fatalf("notifyURL() = %q, want %q", got, tt.want)
			}
		})
	}
	t.Run("pull request wins over failed check", func(t *testing.T) {
		pr := "https://github.com/o/r/pull/7"
		if got := notifyURL(pr, "o", "r", sha, []check{mk("failure", "u1")}); got != pr {
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
