# waitbuild

Waits until all GitHub Actions workflow runs for a commit have finished and
prints their conclusions. Built on [go-github](https://github.com/google/go-github).

## Run on `git push`

Git has no post-push hook, so a repository's pre-push hook (for example
`tree-element/.githooks/pre-push`) starts waitbuild in the background for each
pushed branch. The push is not delayed;
waitbuild waits for the workflow runs of the pushed commit to appear, then
prints a per-workflow status to the terminal and shows a macOS notification.

Install the binary once, somewhere on your PATH:

```sh
cd ~/waitbuild && go build -o ~/.local/bin/waitbuild .
```

Optionally install [terminal-notifier](https://github.com/julienXX/terminal-notifier)
so that clicking the notification opens GitHub: the commit's pull request when
it has one, otherwise the failed run when exactly one failed, otherwise the
commit's checks page. Without it the notification is shown via AppleScript and
is not clickable.

```sh
brew install terminal-notifier
waitbuild -test-notify   # sends a test notification and reports which tool delivered it
```

Then enable the hook in each clone that has one:

```sh
git config core.hooksPath .githooks
```

Set `WAITBUILD_OUT=/path/to/log` to send the hook's output to a file instead of the terminal.

Requires a GitHub token: `GITHUB_TOKEN`, `GH_TOKEN`, or a `gh auth login` session.

## Run by hand

```sh
waitbuild                 # build of HEAD
waitbuild -sha <commit>   # build of a specific commit
waitbuild -pr             # print the URL of the pull request for HEAD
waitbuild -help           # all flags
```

`-pr` prints the pull request that contains the commit (an open one when there
are several) and exits with 1 when there is none.

Exit code 0 when every run succeeded (or was skipped), 1 otherwise.

## License

MIT. See [LICENSE](LICENSE).
