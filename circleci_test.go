package main

import (
	"context"
	"encoding/json"
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

	"github.com/google/go-github/v90/github"
)

// --- fake CircleCI API -----------------------------------------------------

// circlePoll is what the fake CircleCI API reports for one polling round.
type circlePoll struct {
	pipelines []circleCIPipeline
	workflows []circleCIWorkflow // of every pipeline
}

// fakeCircleCI serves the CircleCI endpoints waitbuild uses. The pipeline
// listing answers its n-th request with polls[n] (the last one repeats) and
// the workflow listing answers with the workflows of the latest poll. With
// noProject, every request gets HTTP 404.
type fakeCircleCI struct {
	mu        sync.Mutex
	polls     []circlePoll
	served    int
	noProject bool
	requests  []*http.Request
	srv       *httptest.Server
}

func newFakeCircleCI(t *testing.T, polls ...circlePoll) *fakeCircleCI {
	t.Helper()
	f := &fakeCircleCI{polls: polls}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	orig := circleCIBaseURL
	circleCIBaseURL = f.srv.URL + "/api/v2"
	t.Cleanup(func() { circleCIBaseURL = orig })
	return f
}

func (f *fakeCircleCI) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r)
	if f.noProject {
		http.NotFound(w, r)
		return
	}
	cur := f.polls[min(max(f.served-1, 0), len(f.polls)-1)]
	switch {
	case strings.HasSuffix(r.URL.Path, "/pipeline"):
		cur = f.polls[min(f.served, len(f.polls)-1)]
		f.served++
		_ = json.NewEncoder(w).Encode(circleCIPage[circleCIPipeline]{Items: cur.pipelines})
	case strings.HasSuffix(r.URL.Path, "/workflow"):
		_ = json.NewEncoder(w).Encode(circleCIPage[circleCIWorkflow]{Items: cur.workflows})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeCircleCI) requestPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.requests {
		out = append(out, r.URL.Path)
	}
	return out
}

func mkPipeline(id string, number int64, state, sha string) circleCIPipeline {
	p := circleCIPipeline{ID: id, Number: number, State: state}
	p.VCS.Revision = sha
	return p
}

func mkWorkflow(id, name, status, created string) circleCIWorkflow {
	return circleCIWorkflow{ID: id, Name: name, Status: status, CreatedAt: created}
}

func testCircleCI() *circleCI {
	return &circleCI{token: "ctok", project: "gh/o/r", branch: "feature"}
}

// --- circleCIToken ---------------------------------------------------------

func TestCircleCIToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir()) // no security, so not the real keychain
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("CIRCLE_TOKEN", "")
	t.Setenv("CIRCLECI_TOKEN", "")
	if got := circleCIToken(); got != "" {
		t.Errorf("circleCIToken() = %q without any token, want empty", got)
	}

	write(".circleci/cli.yml", "host: https://circleci.com\ntoken: legacy\n")
	if got := circleCIToken(); got != "legacy" {
		t.Errorf("circleCIToken() = %q, want the legacy CLI token", got)
	}

	write(".config/circleci/config.yml", "host: https://circleci.com\ntoken: \"current\"\n")
	if got := circleCIToken(); got != "current" {
		t.Errorf("circleCIToken() = %q, want the current CLI token", got)
	}

	t.Setenv("CIRCLECI_TOKEN", "from-circleci-token")
	if got := circleCIToken(); got != "from-circleci-token" {
		t.Errorf("circleCIToken() = %q, want CIRCLECI_TOKEN", got)
	}

	t.Setenv("CIRCLE_TOKEN", "from-circle-token")
	if got := circleCIToken(); got != "from-circle-token" {
		t.Errorf("circleCIToken() = %q, want CIRCLE_TOKEN", got)
	}
}

func TestNewCircleCIWithoutToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no security, so not the real keychain
	t.Setenv("CIRCLE_TOKEN", "")
	t.Setenv("CIRCLECI_TOKEN", "")
	if c := newCircleCI("o", "r", "main"); c != nil {
		t.Errorf("newCircleCI() = %+v without a token, want nil", c)
	}
	// A nil client asks nothing.
	checks, found, err := (*circleCI)(nil).checks(context.Background(), "sha")
	if checks != nil || found || err != nil {
		t.Errorf("nil.checks() = %v, %v, %v", checks, found, err)
	}
}

func TestCircleCITokenFromKeychain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake security script needs a POSIX shell")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CIRCLE_TOKEN", "")
	t.Setenv("CIRCLECI_TOKEN", "")
	t.Setenv("CIRCLE_HOST", "")
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	// The fake security knows one item per service and prints its password
	// for "find-generic-password -s <service> -w".
	script := `#!/bin/sh
[ "$1 $2 $4" = "find-generic-password -s -w" ] || exit 2
case "$3" in
"com.circleci.cli:https://circleci.com") echo plain-token ;;
"com.circleci.cli:https://ci.example.com") echo go-keyring-base64:ZW5jb2RlZC10b2tlbg== ;; # "encoded-token"
*) echo "security: The specified item could not be found in the keychain." >&2; exit 44 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "security"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	if got := circleCIToken(); got != "plain-token" {
		t.Errorf("circleCIToken() = %q, want the keychain token for circleci.com", got)
	}

	if err := os.MkdirAll(filepath.Join(home, ".config", "circleci"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config", "circleci", "config.yml"), []byte("host: https://ci.example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := circleCIToken(); got != "encoded-token" {
		t.Errorf("circleCIToken() = %q, want the decoded keychain token for the configured host", got)
	}

	t.Setenv("CIRCLE_HOST", "https://unknown.example.com")
	if got := circleCIToken(); got != "" {
		t.Errorf("circleCIToken() = %q for a host without a keychain item, want empty", got)
	}
}

// --- circleCI.checks -------------------------------------------------------

func TestCircleCIChecks(t *testing.T) {
	ctx := context.Background()

	t.Run("a pipeline not set up yet is one pending check", func(t *testing.T) {
		f := newFakeCircleCI(t, circlePoll{pipelines: []circleCIPipeline{
			mkPipeline("p2", 12, "pending", "sha"),
			mkPipeline("p1", 11, "created", "older"),
		}})
		got, found, err := testCircleCI().checks(ctx, "sha")
		if err != nil || !found {
			t.Fatalf("checks() found=%v err=%v", found, err)
		}
		want := []check{{key: "circleci:pipeline", name: "circleci: pipeline", status: "pending", url: "https://app.circleci.com/pipelines/github/o/r/12"}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("checks() = %+v, want %+v", got, want)
		}
		req := f.requests[0]
		if req.URL.Path != "/api/v2/project/gh/o/r/pipeline" || req.URL.Query().Get("branch") != "feature" {
			t.Errorf("request = %s, want the pipelines of branch feature", req.URL)
		}
		if got := req.Header.Get("Circle-Token"); got != "ctok" {
			t.Errorf("Circle-Token = %q, want ctok", got)
		}
	})

	t.Run("an errored pipeline fails", func(t *testing.T) {
		newFakeCircleCI(t, circlePoll{pipelines: []circleCIPipeline{mkPipeline("p1", 11, "errored", "sha")}})
		got, _, err := testCircleCI().checks(ctx, "sha")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || !got[0].done() || got[0].ok() {
			t.Errorf("checks() = %+v, want one failed check", got)
		}
	})

	t.Run("reports each workflow, newest run of each name", func(t *testing.T) {
		f := newFakeCircleCI(t, circlePoll{
			pipelines: []circleCIPipeline{mkPipeline("p1", 11, "created", "sha")},
			workflows: []circleCIWorkflow{
				mkWorkflow("w3", "test", "running", "2026-10-09T11:40:00Z"), // rerun
				mkWorkflow("w2", "deploy", "not_run", "2026-10-09T11:30:00Z"),
				mkWorkflow("w1", "test", "failed", "2026-10-09T11:30:00Z"),
			},
		})
		got, found, err := testCircleCI().checks(ctx, "sha")
		if err != nil || !found {
			t.Fatalf("checks() found=%v err=%v", found, err)
		}
		want := []check{
			{key: "circleci:test", name: "circleci: test", status: "in_progress", url: "https://app.circleci.com/pipelines/github/o/r/11/workflows/w3"},
			{key: "circleci:deploy", name: "circleci: deploy", status: "completed", conclusion: "skipped", url: "https://app.circleci.com/pipelines/github/o/r/11/workflows/w2"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("checks() = %+v, want %+v", got, want)
		}
		if paths := f.requestPaths(); paths[1] != "/api/v2/pipeline/p1/workflow" {
			t.Errorf("requests = %v, want the workflows of pipeline p1", paths)
		}
	})

	t.Run("no pipeline for the commit", func(t *testing.T) {
		newFakeCircleCI(t, circlePoll{pipelines: []circleCIPipeline{mkPipeline("p1", 11, "created", "other")}})
		got, found, err := testCircleCI().checks(ctx, "sha")
		if got != nil || found || err != nil {
			t.Errorf("checks() = %v, %v, %v; want nothing", got, found, err)
		}
	})

	t.Run("a project unknown to CircleCI is asked only once", func(t *testing.T) {
		f := newFakeCircleCI(t, circlePoll{})
		f.noProject = true
		c := testCircleCI()
		for range 2 {
			got, found, err := c.checks(ctx, "sha")
			if got != nil || found || err != nil {
				t.Errorf("checks() = %v, %v, %v; want nothing", got, found, err)
			}
		}
		if n := len(f.requestPaths()); n != 1 {
			t.Errorf("%d requests, want 1", n)
		}
	})

	t.Run("other API errors fail", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", http.StatusUnauthorized)
		}))
		t.Cleanup(srv.Close)
		orig := circleCIBaseURL
		circleCIBaseURL = srv.URL
		t.Cleanup(func() { circleCIBaseURL = orig })
		_, _, err := testCircleCI().checks(ctx, "sha")
		if err == nil || !strings.Contains(err.Error(), "401") {
			t.Errorf("checks() error = %v, want 401", err)
		}
	})
}

func TestCircleCIWorkflowState(t *testing.T) {
	for _, tc := range []struct {
		status            string
		state, conclusion string
		ok                bool
	}{
		{"running", "in_progress", "", false},
		{"failing", "in_progress", "", false},
		{"success", "completed", "success", true},
		{"on_hold", "completed", "on_hold", true},
		{"not_run", "completed", "skipped", true},
		{"canceled", "completed", "cancelled", false},
		{"failed", "completed", "failure", false},
		{"error", "completed", "error", false},
		{"unauthorized", "completed", "error", false},
		{"something-new", "something-new", "", false},
	} {
		state, conclusion := circleCIWorkflowState(tc.status)
		if state != tc.state || conclusion != tc.conclusion {
			t.Errorf("circleCIWorkflowState(%q) = %q, %q; want %q, %q", tc.status, state, conclusion, tc.state, tc.conclusion)
		}
		if c := (check{status: state, conclusion: conclusion}); c.done() && c.ok() != tc.ok {
			t.Errorf("%s: ok() = %v, want %v", tc.status, c.ok(), tc.ok)
		}
	}
	if c := (check{status: "completed", conclusion: "cancelled"}); !c.cancelled() {
		t.Error("a canceled workflow should count as cancelled")
	}
}

// --- listChecks and waitForChecks with CircleCI ----------------------------

func TestListChecksWithCircleCI(t *testing.T) {
	gh := &poll{
		runs:      []*github.WorkflowRun{done(1, "danger", "success")},
		checkRuns: []*github.CheckRun{mkCheckRun(2, "SonarCloud", "sonarqubecloud", "completed", "success"), mkCheckRun(3, "lint", "circleci-checks", "completed", "success")},
		statuses:  []*github.RepoStatus{mkStatus("ci/circleci: lint", "success"), mkStatus("danger/danger", "success")},
	}

	t.Run("replaces what CircleCI posted to GitHub", func(t *testing.T) {
		f := newFakeAPI(t, gh)
		newFakeCircleCI(t, circlePoll{
			pipelines: []circleCIPipeline{mkPipeline("p1", 11, "created", "sha")},
			workflows: []circleCIWorkflow{mkWorkflow("w1", "test", "running", "2026-10-09T11:30:00Z")},
		})
		got, err := listChecks(context.Background(), f.client(t), testCircleCI(), "o", "r", "sha")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"SonarCloud", "circleci: test", "danger", "danger/danger"}
		if !equalStrings(names(got), want) {
			t.Errorf("names = %v, want %v", names(got), want)
		}
	})

	t.Run("keeps them when CircleCI has no pipeline", func(t *testing.T) {
		f := newFakeAPI(t, gh)
		newFakeCircleCI(t, circlePoll{})
		got, err := listChecks(context.Background(), f.client(t), testCircleCI(), "o", "r", "sha")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"SonarCloud", "ci/circleci: lint", "danger", "danger/danger", "lint"}
		if !equalStrings(names(got), want) {
			t.Errorf("names = %v, want %v", names(got), want)
		}
	})
}

// GitHub's checks all finish while the CircleCI pipeline is still waiting to
// be set up and has reported nothing to GitHub: the build is not finished.
func TestWaitForChecksWaitsForPendingCircleCIPipeline(t *testing.T) {
	f := newFakeAPI(t, runsOnly(done(1, "danger", "success")))
	pending := circlePoll{pipelines: []circleCIPipeline{mkPipeline("p1", 11, "pending", "sha")}}
	running := circlePoll{
		pipelines: []circleCIPipeline{mkPipeline("p1", 11, "created", "sha")},
		workflows: []circleCIWorkflow{mkWorkflow("w1", "test", "running", "2026-10-09T11:30:00Z")},
	}
	finished := circlePoll{
		pipelines: running.pipelines,
		workflows: []circleCIWorkflow{mkWorkflow("w1", "test", "success", "2026-10-09T11:30:00Z")},
	}
	newFakeCircleCI(t, pending, pending, pending, running, finished)

	checks, err := waitForChecks(context.Background(), f.client(t), testCircleCI(), "o", "r", "sha", time.Millisecond, time.Minute, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names(checks), []string{"circleci: test", "danger"}; !equalStrings(got, want) {
		t.Errorf("names = %v, want %v", got, want)
	}
	if n := f.pollCount(); n != 6 {
		t.Errorf("polled %d times, want 6 (3 pending, running, finished twice)", n)
	}
}

func TestRunWithCircleCI(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no gh, no osascript
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WAITBUILD_STATE_DIR", t.TempDir())
	t.Setenv("GITHUB_TOKEN", "tok")
	t.Setenv("CIRCLE_TOKEN", "ctok")
	dir, _, sha := initRepo(t, "https://github.com/mbraak/waitbuild.git")
	t.Chdir(dir)

	f := newFakeAPI(t, runsOnly(done(1, "danger", "success")))
	useFakeAPI(t, f, "tok")
	c := newFakeCircleCI(t, circlePoll{
		pipelines: []circleCIPipeline{mkPipeline("p1", 11, "created", sha)},
		workflows: []circleCIWorkflow{mkWorkflow("w1", "test", "failed", "2026-10-09T11:30:00Z")},
	})

	out := captureStdout(t, func() {
		if err := run("", "", "", false, time.Millisecond, time.Minute, time.Minute, false); err != errFailed {
			t.Errorf("run() = %v, want errFailed", err)
		}
	})
	if !strings.Contains(out, "✘ circleci: test") {
		t.Errorf("output lacks the failed workflow:\n%s", out)
	}
	req := c.requests[0]
	if req.URL.Path != "/api/v2/project/gh/mbraak/waitbuild/pipeline" || req.URL.Query().Get("branch") != "main" {
		t.Errorf("first CircleCI request = %s, want the pipelines of branch main", req.URL)
	}
}
