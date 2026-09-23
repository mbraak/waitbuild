// waitbuild waits until every check reported to GitHub for a commit has
// finished and reports the results: GitHub Actions workflow runs, check runs
// from other GitHub Apps (SonarCloud, Aikido, ...) and commit statuses from
// services such as CircleCI.
//
// It is meant to be started from a repository's pre-push git hook, but can also
// be run by hand:
//
//	waitbuild            # waits for the build of HEAD
//	waitbuild -sha <sha> # waits for the build of a specific commit
//	waitbuild -pr        # prints the URL of the pull request for HEAD
//	waitbuild -fix       # lets Claude Code fix the pull request when the build fails
//
// Authentication: GITHUB_TOKEN or GH_TOKEN, falling back to `gh auth token`.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/google/go-github/v90/github"
)

var successConclusions = map[string]bool{
	"success": true,
	"skipped": true,
	"neutral": true,
}

// actionsAppSlug is the GitHub App that creates check runs for GitHub Actions
// jobs. Those are already covered by the workflow runs, so they are skipped
// when listing check runs to avoid reporting every job twice.
const actionsAppSlug = "github-actions"

// check is one unit of CI reported to GitHub for a commit: a GitHub Actions
// workflow run, a check run created by another GitHub App, or a commit status.
type check struct {
	key        string // stable identity across polls
	name       string
	status     string // as reported by GitHub; "completed" once finished
	conclusion string // set once completed
	url        string
}

// quiet suppresses the progress and result output on stdout (-quiet). Errors
// are still written to stderr and the exit code is unaffected.
var quiet bool

// printf writes progress output to stdout unless -quiet was given. It looks
// up os.Stdout on every call so that tests can redirect it.
func printf(format string, args ...any) {
	if quiet {
		return
	}
	fmt.Fprintf(os.Stdout, format, args...)
}

func (c check) done() bool { return c.status == "completed" }
func (c check) ok() bool   { return successConclusions[c.conclusion] }

// state describes the check for the progress output.
func (c check) state() string {
	if c.done() {
		return "completed (" + c.conclusion + ")"
	}
	return c.status
}

func main() {
	sha := flag.String("sha", "", "commit to wait for (default: HEAD)")
	branch := flag.String("branch", "", "branch name, for display only (default: current branch)")
	interval := flag.Duration("interval", 10*time.Second, "poll interval")
	appearTimeout := flag.Duration("appear-timeout", 3*time.Minute, "how long to wait for the first check to show up")
	timeout := flag.Duration("timeout", 45*time.Minute, "overall timeout")
	notify := flag.Bool("notify", false, "show a desktop notification (macOS) when the build finishes")
	testNotify := flag.Bool("test-notify", false, "send a test desktop notification and exit")
	printPR := flag.Bool("pr", false, "print the URL of the pull request for the commit and exit")
	fix := flag.Bool("fix", false, "when the build fails, let Claude Code fix the pull request and commit its changes")
	flag.BoolVar(&quiet, "quiet", false, "print nothing to the console; only the exit code (and -notify) report the result")
	flag.Parse()

	if *printPR {
		if err := printPullRequest(*sha); err != nil {
			fmt.Fprintln(os.Stderr, "waitbuild:", err)
			os.Exit(1)
		}
		return
	}

	if *testNotify {
		if err := testNotification(); err != nil {
			fmt.Fprintln(os.Stderr, "waitbuild:", err)
			os.Exit(1)
		}
		return
	}

	if err := run(*sha, *branch, *interval, *appearTimeout, *timeout, *notify, *fix); err != nil {
		fmt.Fprintln(os.Stderr, "waitbuild:", err)
		os.Exit(1)
	}
}

func run(sha, branch string, interval, appearTimeout, timeout time.Duration, notify, fix bool) error {
	info, err := repoInfo()
	if err != nil {
		return err
	}
	if sha == "" {
		sha = info.sha
	}
	if branch == "" {
		branch = info.branch
	}
	owner, repo, err := parseGitHubRemote(info.remote)
	if err != nil {
		return err
	}
	token, err := githubToken()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	client, err := newGitHubClient(token)
	if err != nil {
		return fmt.Errorf("creating GitHub client: %w", err)
	}

	hasWorkflows, err := hasActionsWorkflows(ctx, client, owner, repo)
	if err != nil {
		return err
	}
	printf("waitbuild: waiting for the build of %s (%s) in %s/%s\n", branch, sha[:min(10, len(sha))], owner, repo)
	if !hasWorkflows {
		printf("waitbuild: %s/%s has no GitHub Actions workflows, waiting for commit statuses and check runs only\n", owner, repo)
	}

	checks, err := waitForChecks(ctx, client, owner, repo, sha, interval, appearTimeout, hasWorkflows)
	if err != nil {
		return err
	}

	ok := true
	width := nameWidth(checks)
	printf("waitbuild: build of %s finished\n", branch)
	for _, c := range checks {
		mark := "✔"
		if !c.ok() {
			mark = "✘"
			ok = false
		}
		printf("  %s %-*s %-10s %s\n", mark, width, c.name, c.conclusion, c.url)
	}

	var prURL string
	if !ok || notify {
		prURL = pullRequestURL(ctx, client, owner, repo, sha)
	}

	fixed := false
	if !ok && fix {
		if prURL == "" {
			fmt.Fprintf(os.Stderr, "waitbuild: -fix: no pull request found for %s; not running claude\n", sha[:min(10, len(sha))])
		} else if fixed, err = fixBuild(prURL, sha, branch); err != nil {
			fmt.Fprintln(os.Stderr, "waitbuild: -fix:", err)
		}
	}

	if notify {
		title := fmt.Sprintf("Build of %s succeeded", branch)
		if !ok {
			title = fmt.Sprintf("Build of %s FAILED", branch)
		}
		message := fmt.Sprintf("%s/%s @ %s", owner, repo, sha[:min(10, len(sha))])
		if fixed {
			message += ", fix by claude pushed"
		}
		url := notifyURL(prURL, owner, repo, sha, checks)
		desktopNotify(title, message, url, iconFile(ok))
	}
	if !ok {
		return errors.New("one or more checks did not succeed")
	}
	return nil
}

// fixMessage is the commit message for the changes Claude makes with -fix.
const fixMessage = "Fix build"

// fixTrailer marks a commit made by -fix; its value is the commit whose build
// failed. The build of a commit with this trailer is never fixed again, so
// pushing a fix (which starts the pre-push hook, and with it another
// waitbuild) gives at most one attempt per push instead of a loop.
const fixTrailer = "Waitbuild-Fix"

// claudeTools are the commands Claude may run without asking while fixing a
// build: the gh commands for reading the pull request, its checks and the
// logs of failed runs. File edits are allowed through acceptEdits. A project
// can allow more, such as its test command, in its .claude/settings.json.
var claudeTools = []string{
	"Bash(gh pr view:*)",
	"Bash(gh pr checks:*)",
	"Bash(gh pr diff:*)",
	"Bash(gh run view:*)",
}

// fixPrompt asks Claude to fix the failed build of the pull request at prURL.
func fixPrompt(prURL, sha string) string {
	return fmt.Sprintf("The CI build of pull request %s failed for commit %s. "+
		"Look up the failed checks and their logs with gh (for example `gh pr checks %s` and `gh run view <run-id> --log-failed`), "+
		"then fix the cause in this repository with the smallest change that makes the build pass. "+
		"Do not commit or push; the changes are committed and pushed for you.", prURL, sha, prURL)
}

// fixBuild runs Claude Code in print mode in the repository root after a
// failed build, pointing it at the pull request, commits the changes it makes
// and pushes them to the branch. It reports whether a fix was committed.
// Commits Claude makes by itself are folded into that one fix commit, so that
// it always carries fixTrailer. Because waitbuild usually runs in the background after a push, it
// refuses to touch the repository unless HEAD is still sha on branch and the
// working tree is clean, so that work started in the meantime is never swept
// into the commit.
func fixBuild(prURL, sha, branch string) (bool, error) {
	root, err := git("", "rev-parse", "--show-toplevel")
	if err != nil {
		return false, err
	}
	if head, err := git(root, "rev-parse", "HEAD"); err != nil {
		return false, err
	} else if head != sha {
		return false, fmt.Errorf("HEAD is now %s instead of the built commit %s; not running claude", head[:min(10, len(head))], sha[:min(10, len(sha))])
	}
	if current, err := git(root, "rev-parse", "--abbrev-ref", "HEAD"); err != nil {
		return false, err
	} else if current == "HEAD" {
		return false, errors.New("HEAD is detached, so there is no branch to push a fix to; not running claude")
	} else if current != branch {
		return false, fmt.Errorf("the checked out branch is now %s instead of %s; not running claude", current, branch)
	}
	if fixOf, err := git(root, "log", "-1", "--format=%(trailers:key="+fixTrailer+",valueonly)", sha); err != nil {
		return false, err
	} else if fixOf != "" {
		return false, fmt.Errorf("%s is already a fix by claude for %s; not running claude again", sha[:min(10, len(sha))], fixOf[:min(10, len(fixOf))])
	}
	if status, err := git(root, "status", "--porcelain"); err != nil {
		return false, err
	} else if status != "" {
		return false, errors.New("the working tree has uncommitted changes; not running claude")
	}

	printf("waitbuild: asking claude to fix %s\n", prURL)
	// --allowedTools takes a list, so the prompt must come before it.
	args := append([]string{"-p", fixPrompt(prURL, sha), "--permission-mode", "acceptEdits", "--allowedTools"}, claudeTools...)
	if err := runVisible(root, "claude", args...); err != nil {
		return false, fmt.Errorf("claude: %w", err)
	}

	head, err := git(root, "rev-parse", "HEAD")
	if err != nil {
		return false, err
	}
	if head != sha {
		// Claude committed by itself; keep its changes but not its commits.
		if _, err := git(root, "reset", "--quiet", "--soft", sha); err != nil {
			return false, err
		}
	}
	if _, err := git(root, "add", "--all"); err != nil {
		return false, err
	}
	if staged, err := git(root, "status", "--porcelain"); err != nil {
		return false, err
	} else if staged == "" {
		printf("waitbuild: claude made no changes, nothing to commit\n")
		return false, nil
	}
	if _, err := git(root, "commit", "--quiet", "--message", fixMessage, "--trailer", fixTrailer+": "+sha); err != nil {
		return false, err
	}
	commit, err := git(root, "rev-parse", "--short", "HEAD")
	if err != nil {
		return false, err
	}
	if err := runVisible(root, "git", "push", "--quiet", "origin", branch); err != nil {
		return false, fmt.Errorf("committed the fix by claude as %s, but pushing it failed: %w", commit, err)
	}
	printf("waitbuild: pushed the fix by claude as %s to %s\n", commit, branch)
	return true, nil
}

// runVisible runs name with args in dir, with its output on the terminal (or
// stdout discarded with -quiet). The output must not go through a pipe: git
// push runs the pre-push hook, which starts waitbuild in the background, and
// a pipe it inherits would keep Run waiting until that build has finished.
func runVisible(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if !quiet {
		cmd.Stdout = os.Stdout // nil, as with -quiet, means /dev/null
	}
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// git runs git with args in dir (the working directory when empty) and
// returns its trimmed output.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("git %s: %s", args[0], msg)
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(string(out)), nil
}

// nameWidth returns the column width for check names, so that long commit
// status contexts such as "ci/circleci: database-and-api-integrity" line up.
func nameWidth(checks []check) int {
	width := 30
	for _, c := range checks {
		if n := len([]rune(c.name)); n > width {
			width = n
		}
	}
	return width
}

// newGitHubClient is a variable so tests can point the client at a fake server.
var newGitHubClient = func(token string) (*github.Client, error) {
	return github.NewClient(github.WithAuthToken(token))
}

// hasActionsWorkflows reports whether the repository has any GitHub Actions
// workflows. A repository without them can still have a build, reported as
// commit statuses (CircleCI) or check runs (other GitHub Apps), so this only
// tunes the output and the message shown when nothing appears.
func hasActionsWorkflows(ctx context.Context, client *github.Client, owner, repo string) (bool, error) {
	wfs, _, err := client.Actions.ListWorkflows(ctx, owner, repo, &github.ListOptions{PerPage: 1})
	if err != nil {
		return false, fmt.Errorf("listing workflows: %w", err)
	}
	return wfs.GetTotalCount() > 0, nil
}

// waitForChecks polls until at least one check exists for sha and every check
// has completed. Because several workflows and services report on the same
// push and can register a few seconds apart, "all completed" has to hold for
// two consecutive polls before the result is accepted.
func waitForChecks(ctx context.Context, client *github.Client, owner, repo, sha string, interval, appearTimeout time.Duration, hasWorkflows bool) ([]check, error) {
	start := time.Now()
	seen := map[string]string{} // check key -> last reported state
	settledOnce := false
	width := 30

	for {
		checks, err := listChecks(ctx, client, owner, repo, sha)
		if err != nil {
			return nil, err
		}

		if len(checks) == 0 && time.Since(start) > appearTimeout {
			hint := "was the commit pushed?"
			if !hasWorkflows {
				hint = "the repository has no GitHub Actions workflows; was the commit pushed and is CI configured?"
			}
			return nil, fmt.Errorf("no checks appeared for %s within %s (%s)", sha, appearTimeout, hint)
		}

		if w := nameWidth(checks); w > width {
			width = w
		}
		allDone := len(checks) > 0
		for _, c := range checks {
			if !c.done() {
				allDone = false
			}
			if state := c.state(); seen[c.key] != state {
				seen[c.key] = state
				printf("  %-*s %s\n", width, c.name, state)
			}
		}

		if allDone {
			if settledOnce {
				return checks, nil
			}
			settledOnce = true
		} else {
			settledOnce = false
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("gave up waiting: %w", ctx.Err())
		case <-time.After(interval):
		}
	}
}

// listChecks gathers everything GitHub knows about the commit's CI: Actions
// workflow runs, check runs from other GitHub Apps and commit statuses. The
// result is sorted by name.
func listChecks(ctx context.Context, client *github.Client, owner, repo, sha string) ([]check, error) {
	runs, err := listRuns(ctx, client, owner, repo, sha)
	if err != nil {
		return nil, err
	}
	checkRuns, err := listCheckRuns(ctx, client, owner, repo, sha)
	if err != nil {
		return nil, err
	}
	statuses, err := listStatuses(ctx, client, owner, repo, sha)
	if err != nil {
		return nil, err
	}

	all := append(append(runs, checkRuns...), statuses...)
	sort.SliceStable(all, func(i, j int) bool { return all[i].name < all[j].name })
	return all, nil
}

// listRuns returns the GitHub Actions workflow runs for sha.
func listRuns(ctx context.Context, client *github.Client, owner, repo, sha string) ([]check, error) {
	var all []check
	opts := &github.ListWorkflowRunsOptions{
		HeadSHA:     sha,
		ListOptions: github.ListOptions{PerPage: 100},
	}
	for {
		runs, resp, err := client.Actions.ListRepositoryWorkflowRuns(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("listing workflow runs: %w", err)
		}
		for _, r := range runs.WorkflowRuns {
			all = append(all, check{
				key:        fmt.Sprintf("run:%d", r.GetID()),
				name:       r.GetName(),
				status:     r.GetStatus(),
				conclusion: r.GetConclusion(),
				url:        r.GetHTMLURL(),
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return all, nil
}

// listCheckRuns returns the check runs for sha created by GitHub Apps other
// than GitHub Actions, whose runs listRuns already reports per workflow.
func listCheckRuns(ctx context.Context, client *github.Client, owner, repo, sha string) ([]check, error) {
	var all []check
	opts := &github.ListCheckRunsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	for {
		res, resp, err := client.Checks.ListCheckRunsForRef(ctx, owner, repo, sha, opts)
		if err != nil {
			return nil, fmt.Errorf("listing check runs: %w", err)
		}
		for _, r := range res.CheckRuns {
			if r.GetApp().GetSlug() == actionsAppSlug {
				continue
			}
			all = append(all, check{
				key:        fmt.Sprintf("check:%d", r.GetID()),
				name:       r.GetName(),
				status:     r.GetStatus(),
				conclusion: r.GetConclusion(),
				url:        r.GetHTMLURL(),
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return all, nil
}

// listStatuses returns the commit statuses for sha, one per context (for
// example "ci/circleci: lint"). The combined status endpoint already reduces
// each context to its latest status. A status is "completed" unless pending,
// and its state (success, failure or error) doubles as the conclusion.
func listStatuses(ctx context.Context, client *github.Client, owner, repo, sha string) ([]check, error) {
	var all []check
	opts := &github.ListOptions{PerPage: 100}
	for {
		combined, resp, err := client.Repositories.GetCombinedStatus(ctx, owner, repo, sha, opts)
		if err != nil {
			return nil, fmt.Errorf("listing commit statuses: %w", err)
		}
		for _, s := range combined.Statuses {
			c := check{
				key:    "status:" + s.GetContext(),
				name:   s.GetContext(),
				status: "pending",
				url:    s.GetTargetURL(),
			}
			if s.GetState() != "pending" {
				c.status = "completed"
				c.conclusion = s.GetState()
			}
			all = append(all, c)
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return all, nil
}

var remoteRE = regexp.MustCompile(`github\.com[:/]([^/]+)/([^/]+?)(?:\.git)?/?$`)

func parseGitHubRemote(url string) (owner, repo string, err error) {
	m := remoteRE.FindStringSubmatch(strings.TrimSpace(url))
	if m == nil {
		return "", "", fmt.Errorf("origin %q is not a github.com remote", url)
	}
	return m[1], m[2], nil
}

func githubToken() (string, error) {
	for _, env := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if t := os.Getenv(env); t != "" {
			return t, nil
		}
	}
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return "", errors.New("no GitHub token: set GITHUB_TOKEN or log in with `gh auth login`")
	}
	return strings.TrimSpace(string(out)), nil
}

// gitInfo describes the repository the current directory belongs to.
type gitInfo struct {
	sha    string // full hash of HEAD
	branch string // short branch name, or "HEAD" when detached
	remote string // first URL of the "origin" remote
}

// repoInfo opens the repository containing the working directory (searching
// parent directories like git does) and reads HEAD and the origin remote.
func repoInfo() (gitInfo, error) {
	repo, err := gogit.PlainOpenWithOptions(".", &gogit.PlainOpenOptions{
		DetectDotGit:          true,
		EnableDotGitCommonDir: true,
	})
	if err != nil {
		return gitInfo{}, fmt.Errorf("opening git repository: %w", err)
	}

	head, err := repo.Head()
	if err != nil {
		return gitInfo{}, fmt.Errorf("reading HEAD: %w", err)
	}
	info := gitInfo{sha: head.Hash().String(), branch: "HEAD"}
	if head.Name().IsBranch() {
		info.branch = head.Name().Short()
	}

	remote, err := repo.Remote("origin")
	if err != nil {
		return gitInfo{}, fmt.Errorf("reading remote origin: %w", err)
	}
	urls := remote.Config().URLs
	if len(urls) == 0 {
		return gitInfo{}, errors.New("remote origin has no URL")
	}
	info.remote = urls[0]
	return info, nil
}

// printPullRequest prints the URL of the pull request that contains sha
// (HEAD when empty). It fails when the commit has no pull request.
func printPullRequest(sha string) error {
	info, err := repoInfo()
	if err != nil {
		return err
	}
	if sha == "" {
		sha = info.sha
	}
	owner, repo, err := parseGitHubRemote(info.remote)
	if err != nil {
		return err
	}
	token, err := githubToken()
	if err != nil {
		return err
	}
	client, err := newGitHubClient(token)
	if err != nil {
		return fmt.Errorf("creating GitHub client: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	url := pullRequestURL(ctx, client, owner, repo, sha)
	if url == "" {
		return fmt.Errorf("no pull request found for %s in %s/%s", sha[:min(10, len(sha))], owner, repo)
	}
	fmt.Println(url)
	return nil
}

// pullRequestURL returns the GitHub page of the pull request that contains
// sha, preferring an open one. It returns "" when there is no such pull
// request or the lookup fails; the notification then falls back to another
// page, so a lookup failure is never fatal.
func pullRequestURL(ctx context.Context, client *github.Client, owner, repo, sha string) string {
	prs, _, err := client.PullRequests.ListPullRequestsWithCommit(ctx, owner, repo, sha, &github.ListOptions{PerPage: 100})
	if err != nil || len(prs) == 0 {
		return ""
	}
	for _, pr := range prs {
		if pr.GetState() == "open" && pr.GetHTMLURL() != "" {
			return pr.GetHTMLURL()
		}
	}
	return prs[0].GetHTMLURL()
}

// notifyURL picks the page a notification should open: the commit's pull
// request when it has one, otherwise the single failed check when there is
// exactly one, otherwise the commit's checks page on GitHub.
func notifyURL(prURL, owner, repo, sha string, checks []check) string {
	if prURL != "" {
		return prURL
	}
	var failed []check
	for _, c := range checks {
		if !c.ok() {
			failed = append(failed, c)
		}
	}
	if len(failed) == 1 && failed[0].url != "" {
		return failed[0].url
	}
	return fmt.Sprintf("https://github.com/%s/%s/commit/%s/checks", owner, repo, sha)
}

// testNotification sends a notification through the same path run uses and
// reports which tool delivered it, so the setup can be checked without a push.
func testNotification() error {
	url := "https://github.com/mbraak/waitbuild"
	switch desktopNotify("waitbuild test", "Click to open GitHub", url, iconFile(true)) {
	case "terminal-notifier":
		fmt.Println("waitbuild: notification sent via terminal-notifier; clicking it opens", url)
	case "osascript":
		fmt.Println("waitbuild: notification sent via AppleScript (not clickable)")
		fmt.Println("waitbuild: install terminal-notifier (brew install terminal-notifier) and allow it in System Settings > Notifications to make notifications open GitHub")
	default:
		return errors.New("no notification tool found (terminal-notifier or osascript)")
	}
	return nil
}

// desktopNotify shows a macOS notification and returns the name of the tool
// that delivered it, or "" if none did. With terminal-notifier installed
// (brew install terminal-notifier) clicking the notification opens url and
// icon (a PNG path, may be "") is shown next to the text; otherwise it falls
// back to AppleScript, which cannot attach a click action or an image.
func desktopNotify(title, message, url, icon string) string {
	if tn, err := exec.LookPath("terminal-notifier"); err == nil {
		args := []string{"-title", title, "-message", message, "-group", "waitbuild"}
		if url != "" {
			args = append(args, "-open", url)
		}
		if icon != "" {
			args = append(args, "-contentImage", icon)
		}
		if exec.Command(tn, args...).Run() == nil {
			return "terminal-notifier"
		}
	}
	if _, err := exec.LookPath("osascript"); err != nil {
		return ""
	}
	script := fmt.Sprintf(`display notification %q with title %q`, message, title)
	if exec.Command("osascript", "-e", script).Run() != nil {
		return ""
	}
	return "osascript"
}

// iconFile returns the path of the notification icon for a successful (green
// check mark) or failed (red cross) build, rendering it into the user's cache
// directory the first time. It returns "" when the icon cannot be written, in
// which case the notification is shown without an image.
func iconFile(ok bool) string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	name := "failure.png"
	if ok {
		name = "success.png"
	}
	path := filepath.Join(dir, "waitbuild", name)
	if _, err := os.Stat(path); err == nil {
		return path
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return ""
	}
	f, err := os.Create(path)
	if err != nil {
		return ""
	}
	if err := png.Encode(f, drawIcon(ok)); err != nil {
		f.Close()
		os.Remove(path)
		return ""
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return ""
	}
	return path
}

// drawIcon renders a filled circle, green with a white check mark for ok and
// red with a white cross otherwise.
func drawIcon(ok bool) *image.RGBA {
	const size = 128
	fill := color.RGBA{0xcf, 0x22, 0x2e, 0xff} // red
	var strokes [][4]float64
	if ok {
		fill = color.RGBA{0x2d, 0xa4, 0x4e, 0xff} // green
		strokes = [][4]float64{{34, 66, 56, 88}, {56, 88, 96, 44}}
	} else {
		strokes = [][4]float64{{42, 42, 86, 86}, {86, 42, 42, 86}}
	}
	const (
		center = size / 2.0
		radius = size/2.0 - 2
		stroke = 7.0 // half the line width
	)
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			px, py := float64(x)+0.5, float64(y)+0.5
			// Coverage 0..1 with a one-pixel soft edge, for anti-aliasing.
			cover := func(d float64) float64 { return math.Max(0, math.Min(1, 0.5-d)) }
			disc := cover(math.Hypot(px-center, py-center) - radius)
			if disc == 0 {
				continue
			}
			mark := 0.0
			for _, s := range strokes {
				mark = math.Max(mark, cover(distToSegment(px, py, s)-stroke))
			}
			c := blend(fill, color.RGBA{0xff, 0xff, 0xff, 0xff}, mark)
			c.A = uint8(math.Round(disc * 255))
			c.R = uint8(math.Round(float64(c.R) * disc))
			c.G = uint8(math.Round(float64(c.G) * disc))
			c.B = uint8(math.Round(float64(c.B) * disc))
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

// distToSegment returns the distance from (px, py) to the segment s = {x1, y1, x2, y2}.
func distToSegment(px, py float64, s [4]float64) float64 {
	dx, dy := s[2]-s[0], s[3]-s[1]
	t := ((px-s[0])*dx + (py-s[1])*dy) / (dx*dx + dy*dy)
	t = math.Max(0, math.Min(1, t))
	return math.Hypot(px-(s[0]+t*dx), py-(s[1]+t*dy))
}

// blend mixes a and b, with t = 0 giving a and t = 1 giving b.
func blend(a, b color.RGBA, t float64) color.RGBA {
	mix := func(x, y uint8) uint8 { return uint8(math.Round(float64(x)*(1-t) + float64(y)*t)) }
	return color.RGBA{mix(a.R, b.R), mix(a.G, b.G), mix(a.B, b.B), 0xff}
}
