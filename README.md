# waitbuild

Waits until every check GitHub reports for a commit has finished and prints
the results. It covers GitHub Actions workflow runs, check runs from other
GitHub Apps (SonarCloud, Aikido, ...) and commit statuses from services such
as CircleCI (`ci/circleci: <job>`). Built on
[go-github](https://github.com/google/go-github).

## Run on `git push`

Git has no post-push hook, so a repository's pre-push hook (for example
`tree-element/.githooks/pre-push`) starts waitbuild in the background for each
pushed branch. The push is not delayed;
waitbuild waits for the checks of the pushed commit to appear, then
prints a per-check status to the terminal and shows a macOS notification.

Install the binary once, somewhere on your PATH:

```sh
cd ~/waitbuild && go build -o ~/.local/bin/waitbuild .
```

Optionally install [terminal-notifier](https://github.com/julienXX/terminal-notifier)
so that the notification shows a green check mark or a red cross for the build
result, and clicking it opens GitHub: the commit's pull request when it has
one, otherwise the failed run when exactly one failed, otherwise the commit's
checks page. The icons are rendered once into `~/Library/Caches/waitbuild`.
Without terminal-notifier the notification is shown via AppleScript, without an
icon and not clickable.

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
waitbuild -quiet -notify  # print nothing; report via notification and exit code
waitbuild -fix            # when the build fails, let Claude Code fix the pull request
waitbuild -help           # all flags
```

`-pr` prints the pull request that contains the commit (an open one when there
are several) and exits with 1 when there is none.

`-fix` hands a failed build to [Claude Code](https://claude.com/claude-code):
it runs `claude -p` in the repository root with a prompt that points at the
commit's pull request, lets Claude look up the failed checks and their logs
with `gh`, and commits the changes it makes as `Fix build`. The commit is not
pushed; push it to rebuild. A commit without a pull request is not fixed.

It only runs when the built commit is still `HEAD` on the same branch and the
working tree is clean, so that work started after the push is never swept
into the commit. Nothing is committed when Claude fails or changes nothing; a
commit Claude makes by itself is kept as is. The exit code stays 1, because
the pushed build did fail.

Claude runs with `--permission-mode acceptEdits` and may run `gh pr view`,
`gh pr checks`, `gh pr diff` and `gh run view` without asking. It cannot ask
for anything else in print mode, so allow further commands, such as the
project's tests, in the project's `.claude/settings.json`.

`-quiet` suppresses all output on stdout; errors are still written to stderr.

Exit code 0 when every check succeeded (or was skipped), 1 otherwise. A commit
status in state `failure` or `error` counts as a failed check.

A repository without GitHub Actions workflows is fine: waitbuild then waits for
commit statuses and check runs only. When nothing at all is reported within
`-appear-timeout` (3 minutes by default) it gives up.

## License

MIT. See [LICENSE](LICENSE).
