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
		desktopNotify(title, fmt.Sprintf("%s/%s @ %s", owner, repo, sha[:min(10, len(sha))]), url, iconFile(ok))
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
