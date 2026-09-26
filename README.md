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
waitbuild -help           # all flags
```

`-pr` prints the pull request that contains the commit (an open one when there
are several) and exits with 1 when there is none.

`-quiet` suppresses all output on stdout; errors are still written to stderr.

Exit code 0 when every check succeeded (or was skipped), 1 otherwise. A commit
status in state `failure` or `error` counts as a failed check.

A repository without GitHub Actions workflows is fine: waitbuild then waits for
commit statuses and check runs only. When nothing at all is reported within
`-appear-timeout` (3 minutes by default) it gives up.

## Menu bar app

`waitbuild-menubar` shows the builds waitbuild is waiting for in the macOS menu
bar: ⏳ with the number of running builds, or ✅ / ❌ for the most recent
result. Each build has a submenu with its checks, and clicking a check opens
it on GitHub. Finished builds stay listed until you dismiss them, use
"Clear finished", or they are older than `-keep` (24 hours by default).

Every waitbuild process records its build in a JSON file in
`~/Library/Caches/waitbuild/watches` (override with `WAITBUILD_STATE_DIR`).
The menu bar app reads these files, so it shows builds started from any
terminal or git hook, and builds started before the app was launched.

A build that failed can be rerun on GitHub. Once a minute (`-rerun-interval`)
the app runs `waitbuild -if-rerun` for every build that failed, gave up or
was stopped. That call exits right away while the build is unchanged. When a
check is queued or running again, or every check now succeeds because the
rerun already finished, it waits for the build like any other waitbuild
process: the menu shows the new result and you get a notification. The app looks for `waitbuild` next to its own
binary, then on `PATH` (override with `-waitbuild`). The app itself makes no
GitHub API calls.

```sh
cd ~/waitbuild && go build -o ~/.local/bin/waitbuild-menubar ./cmd/waitbuild-menubar
waitbuild-menubar &
```

To start it at login, install a LaunchAgent. This writes the plist with your
home directory filled in and loads it:

```sh
cat > ~/Library/LaunchAgents/com.github.mbraak.waitbuild-menubar.plist <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.github.mbraak.waitbuild-menubar</string>
  <key>ProgramArguments</key>
  <array>
    <string>$HOME/.local/bin/waitbuild-menubar</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <!-- lets waitbuild find gh for the GitHub token when checking for reruns -->
    <key>PATH</key>
    <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
</dict>
</plist>
EOF
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.github.mbraak.waitbuild-menubar.plist
```

Check that it is running with
`launchctl print gui/$(id -u)/com.github.mbraak.waitbuild-menubar`. After
changing the plist, run
`launchctl bootout gui/$(id -u) ~/Library/LaunchAgents/com.github.mbraak.waitbuild-menubar.plist`
and bootstrap it again.

## License

MIT. See [LICENSE](LICENSE).
