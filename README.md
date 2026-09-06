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
waitbuild -help           # all flags
```

Exit code 0 when every run succeeded (or was skipped), 1 otherwise.
