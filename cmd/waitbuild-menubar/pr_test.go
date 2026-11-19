package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPullRequestFromOutput(t *testing.T) {
	tests := map[string]string{
		"https://github.com/o/r/pull/3\n":       "https://github.com/o/r/pull/3",
		"":                                      "",
		"not a url\n":                           "",
		"https://evil.example/pull/3\n":         "",
		"https://github.com/o/r/pull/3\nmore\n": "",
	}
	for out, want := range tests {
		if got := pullRequestFromOutput([]byte(out)); got != want {
			t.Errorf("pullRequestFromOutput(%q) = %q, want %q", out, got, want)
		}
	}
}

func TestFindPullRequest(t *testing.T) {
	dir := t.TempDir()
	write := func(script string) string {
		p := filepath.Join(dir, "waitbuild")
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}

	bin := write(`[ "$*" = "-pr -repo o/r -sha abc" ] && echo https://github.com/o/r/pull/7`)
	if got := findPullRequest(bin, "o/r", "abc"); got != "https://github.com/o/r/pull/7" {
		t.Errorf("findPullRequest = %q, want the pull request", got)
	}
	bin = write("echo 'waitbuild: no pull request found' >&2; exit 1")
	if got := findPullRequest(bin, "o/r", "abc"); got != "" {
		t.Errorf("findPullRequest without a pull request = %q, want empty", got)
	}
	if got := findPullRequest(filepath.Join(dir, "missing"), "o/r", "abc"); got != "" {
		t.Errorf("findPullRequest without waitbuild = %q, want empty", got)
	}
}

func TestOpenOnGitHubAction(t *testing.T) {
	opened := make(chan string, 1)
	orig := openURL
	openURL = func(url string) { opened <- url }
	t.Cleanup(func() { openURL = orig })

	it := item{title: "Open on GitHub", url: "https://github.com/o/r/commit/abc/checks", pullRequestOf: &commit{repo: "o/r", sha: "abc"}}
	wait := func() string {
		select {
		case url := <-opened:
			return url
		case <-time.After(2 * time.Second):
			t.Fatal("nothing opened")
			return ""
		}
	}

	a := &app{findPR: func(repo, sha string) string {
		if repo == "o/r" && sha == "abc" {
			return "https://github.com/o/r/pull/7"
		}
		return ""
	}}
	a.action(it)()
	if got := wait(); got != "https://github.com/o/r/pull/7" {
		t.Errorf("opened %q, want the pull request", got)
	}

	a.findPR = func(string, string) string { return "" }
	a.action(it)()
	if got := wait(); got != it.url {
		t.Errorf("without a pull request opened %q, want %q", got, it.url)
	}

	a.findPR = nil // waitbuild not found
	a.action(it)()
	if got := wait(); got != it.url {
		t.Errorf("without waitbuild opened %q, want %q", got, it.url)
	}
}
