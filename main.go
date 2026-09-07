// waitbuild waits until all GitHub Actions workflow runs for a commit have
// finished and reports their conclusions.
//
// It is meant to be started from a repository's pre-push git hook, but can also
// be run by hand:
//
//	waitbuild            # waits for the build of HEAD
//	waitbuild -sha <sha> # waits for the build of a specific commit
//	waitbuild -pr        # prints the URL of the pull request for HEAD
//
// Authentication: GITHUB_TOKEN or GH_TOKEN, falling back to `gh auth token`.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
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

func main() {
	sha := flag.String("sha", "", "commit to wait for (default: HEAD)")
	branch := flag.String("branch", "", "branch name, for display only (default: current branch)")
	interval := flag.Duration("interval", 10*time.Second, "poll interval")
	appearTimeout := flag.Duration("appear-timeout", 3*time.Minute, "how long to wait for the first workflow run to show up")
	timeout := flag.Duration("timeout", 45*time.Minute, "overall timeout")
	notify := flag.Bool("notify", false, "show a desktop notification (macOS) when the build finishes")
	testNotify := flag.Bool("test-notify", false, "send a test desktop notification and exit")
	printPR := flag.Bool("pr", false, "print the URL of the pull request for the commit and exit")
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

	if err := run(*sha, *branch, *interval, *appearTimeout, *timeout, *notify); err != nil {
		fmt.Fprintln(os.Stderr, "waitbuild:", err)
		os.Exit(1)
	}
}

func run(sha, branch string, interval, appearTimeout, timeout time.Duration, notify bool) error {
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

	fmt.Printf("waitbuild: waiting for the build of %s (%s) in %s/%s\n", branch, sha[:min(10, len(sha))], owner, repo)

	runs, err := waitForRuns(ctx, client, owner, repo, sha, interval, appearTimeout)
	if err != nil {
		return err
	}

	ok := true
	fmt.Printf("waitbuild: build of %s finished\n", branch)
	for _, r := range runs {
		mark := "✔"
		if !successConclusions[r.GetConclusion()] {
			mark = "✘"
			ok = false
		}
		fmt.Printf("  %s %-30s %-10s %s\n", mark, r.GetName(), r.GetConclusion(), r.GetHTMLURL())
	}

	if notify {
		title := fmt.Sprintf("Build of %s succeeded", branch)
		if !ok {
			title = fmt.Sprintf("Build of %s FAILED", branch)
		}
		url := notifyURL(pullRequestURL(ctx, client, owner, repo, sha), owner, repo, sha, runs)
		desktopNotify(title, fmt.Sprintf("%s/%s @ %s", owner, repo, sha[:min(10, len(sha))]), url)
	}
	if !ok {
		return errors.New("one or more workflow runs did not succeed")
	}
	return nil
}

// newGitHubClient is a variable so tests can point the client at a fake server.
var newGitHubClient = func(token string) (*github.Client, error) {
	return github.NewClient(github.WithAuthToken(token))
}

// waitForRuns polls until at least one workflow run exists for sha and every
// run has completed. Because several workflows are triggered by the same push
// and can register a few seconds apart, "all completed" has to hold for two
// consecutive polls before the result is accepted.
func waitForRuns(ctx context.Context, client *github.Client, owner, repo, sha string, interval, appearTimeout time.Duration) ([]*github.WorkflowRun, error) {
	start := time.Now()
	seen := map[int64]string{} // run id -> last reported state
	settledOnce := false

	for {
		runs, err := listRuns(ctx, client, owner, repo, sha)
		if err != nil {
			return nil, err
		}

		if len(runs) == 0 {
			if time.Since(start) > appearTimeout {
				return nil, fmt.Errorf("no workflow runs appeared for %s within %s (was the commit pushed?)", sha, appearTimeout)
			}
		}

		allDone := len(runs) > 0
		for _, r := range runs {
			state := r.GetStatus()
			if state == "completed" {
				state = "completed (" + r.GetConclusion() + ")"
			} else {
				allDone = false
			}
			if seen[r.GetID()] != state {
				seen[r.GetID()] = state
				fmt.Printf("  %-30s %s\n", r.GetName(), state)
			}
		}

		if allDone {
			if settledOnce {
				return runs, nil
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

func listRuns(ctx context.Context, client *github.Client, owner, repo, sha string) ([]*github.WorkflowRun, error) {
	var all []*github.WorkflowRun
	opts := &github.ListWorkflowRunsOptions{
		HeadSHA:     sha,
		ListOptions: github.ListOptions{PerPage: 100},
	}
	for {
		runs, resp, err := client.Actions.ListRepositoryWorkflowRuns(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("listing workflow runs: %w", err)
		}
		all = append(all, runs.WorkflowRuns...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	sort.Slice(all, func(i, j int) bool { return all[i].GetName() < all[j].GetName() })
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
// request when it has one, otherwise the single failed run when there is
// exactly one, otherwise the commit's checks page on GitHub.
func notifyURL(prURL, owner, repo, sha string, runs []*github.WorkflowRun) string {
	if prURL != "" {
		return prURL
	}
	var failed []*github.WorkflowRun
	for _, r := range runs {
		if !successConclusions[r.GetConclusion()] {
			failed = append(failed, r)
		}
	}
	if len(failed) == 1 && failed[0].GetHTMLURL() != "" {
		return failed[0].GetHTMLURL()
	}
	return fmt.Sprintf("https://github.com/%s/%s/commit/%s/checks", owner, repo, sha)
}

// testNotification sends a notification through the same path run uses and
// reports which tool delivered it, so the setup can be checked without a push.
func testNotification() error {
	url := "https://github.com/mbraak/waitbuild"
	switch desktopNotify("waitbuild test", "Click to open GitHub", url) {
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
// (brew install terminal-notifier) clicking the notification opens url;
// otherwise it falls back to AppleScript, which cannot attach a click action.
func desktopNotify(title, message, url string) string {
	if tn, err := exec.LookPath("terminal-notifier"); err == nil {
		args := []string{"-title", title, "-message", message, "-group", "waitbuild"}
		if url != "" {
			args = append(args, "-open", url)
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
