// waitbuild waits until all GitHub Actions workflow runs for a commit have
// finished and reports their conclusions.
//
// It is meant to be started from a repository's pre-push git hook, but can also
// be run by hand:
//
//	waitbuild            # waits for the build of HEAD
//	waitbuild -sha <sha> # waits for the build of a specific commit
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
	flag.Parse()

	if err := run(*sha, *branch, *interval, *appearTimeout, *timeout, *notify); err != nil {
		fmt.Fprintln(os.Stderr, "waitbuild:", err)
		os.Exit(1)
	}
}

func run(sha, branch string, interval, appearTimeout, timeout time.Duration, notify bool) error {
	var err error
	if sha == "" {
		if sha, err = git("rev-parse", "HEAD"); err != nil {
			return err
		}
	}
	if branch == "" {
		if branch, err = git("rev-parse", "--abbrev-ref", "HEAD"); err != nil {
			return err
		}
	}
	remote, err := git("remote", "get-url", "origin")
	if err != nil {
		return err
	}
	owner, repo, err := parseGitHubRemote(remote)
	if err != nil {
		return err
	}
	token, err := githubToken()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	client, err := github.NewClient(github.WithAuthToken(token))
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
		desktopNotify(title, fmt.Sprintf("%s/%s @ %s", owner, repo, sha[:min(10, len(sha))]))
	}
	if !ok {
		return errors.New("one or more workflow runs did not succeed")
	}
	return nil
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

func git(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func desktopNotify(title, message string) {
	if _, err := exec.LookPath("osascript"); err != nil {
		return
	}
	script := fmt.Sprintf(`display notification %q with title %q`, message, title)
	_ = exec.Command("osascript", "-e", script).Run()
}
