# pr-jump

Type `pr 5219` in your browser's address bar and go straight to pull request #5219, whichever repository it is in.

pr-jump is a small local server that keeps an index of every pull request owned by the GitHub users and organisations you choose. Your browser sends the query to the server, which redirects you to the matching PR on GitHub. A PR that is already in the index opens in a few milliseconds, because the server does not need to ask GitHub.

| You type | You go to |
|---|---|
| `pr 5219` or `pr #5219` | the newest PR numbered 5219 in any indexed repository |
| `pr 42 ios`, `pr ios#42` or `pr ios/42` | PR 42 in the repository called `ios` |
| `pr acme/ios#42` | PR 42 in `acme/ios`, when several owners have an `ios` repository |
| `pr 42 v12` | PR 42 in the newest repository whose name contains `v12` |
| `pr flaky tests` | GitHub's PR search across your owners |

When several repositories have a PR with the same number, the newest PR wins. Small numbers are often shared by many repositories, so add part of the repository name to choose one.

## Requirements

- Python 3.9 or later. pr-jump uses only the standard library.
- The [GitHub CLI](https://cli.github.com/), signed in with `gh auth login`, or a `GITHUB_TOKEN` environment variable. The token needs read access to the repositories you index. If your organisation uses SAML single sign-on, authorise the token for it.

## Install

Clone the repository somewhere it can stay, because the service runs the script from there:

```sh
git clone https://github.com/beastawakens/pr-jump.git ~/src/pr-jump
```

Then set it up to start when you log in. Replace `my-org` with your GitHub organisation or username. Repeat `--owner` to index more than one.

### macOS

```sh
~/src/pr-jump/install-macos.sh --owner my-org --owner my-username
```

The installer checks the owners exist, then creates a launch agent called `local.pr-jump` and starts it. Run the script again to change the owners or port.

The launch agent gets its token from `gh`, so the installer needs `gh` signed in. It cannot see a `GITHUB_TOKEN` set in your shell. To remove it, run `~/src/pr-jump/install-macos.sh --uninstall`.

### Linux

Create `~/.config/systemd/user/pr-jump.service`:

```ini
[Unit]
Description=pr-jump

[Service]
ExecStart=/usr/bin/python3 %h/src/pr-jump/pr_jump.py --owner my-org
Restart=always

[Install]
WantedBy=default.target
```

Then start it:

```sh
systemctl --user daemon-reload
systemctl --user enable --now pr-jump
```

If `gh` is not in `/usr/bin` or `/usr/local/bin`, add `Environment=PATH=/path/to/gh/dir:/usr/bin:/bin` under `[Service]`.

### Windows

Use Task Scheduler to run this when you log on:

```
pythonw C:\path\to\pr-jump\pr_jump.py --owner my-org
```

### Any system, by hand

```sh
python3 pr_jump.py --owner my-org
```

The first index takes about a minute for 25,000 PRs. Until it finishes, lookups go to GitHub's search page instead.

If an owner does not exist, pr-jump stops with an error. Check the owners with `--validate` first.

## Set up your browser

Each browser needs a search shortcut that sends what you type after `pr` to this address:

```
http://127.0.0.1:7777/?q=%s
```

`%s` stands for what you type. If you chose another port, change `7777` to match. You can use any shortcut you like instead of `pr`.

### Chrome, Brave, Edge and Opera

1. Open the search engine settings by typing one of these into the address bar:
   - Chrome: `chrome://settings/searchEngines`
   - Brave: `brave://settings/searchEngines`
   - Edge: `edge://settings/searchEngines`
   - Opera: `opera://settings/searchEngines`
2. Find site search (Chrome and Brave call it "Site search"), then select Add.
3. Fill in the form:
   - Name: `PR jump`
   - Shortcut (Opera calls it Keyword): `pr`
   - URL: `http://127.0.0.1:7777/?q=%s`
4. Save it.

To use it, type `pr`, then a space or Tab, then the PR number, then press Enter.

### Vivaldi

1. Open Settings, then Search.
2. Add a search engine:
   - Name: `PR jump`
   - Nickname: `pr`
   - URL: `http://127.0.0.1:7777/?q=%s`
3. Type `pr 5219` in the address bar.

### Firefox

Firefox uses bookmark keywords:

1. Open the bookmarks manager with Ctrl+Shift+O (Cmd+Shift+O on a Mac).
2. Choose Organise (Organize in US English), then Add bookmark.
3. Fill in the form:
   - Name: `PR jump`
   - URL: `http://127.0.0.1:7777/?q=%s`
   - Keyword: `pr`
4. Save it, then type `pr 5219` in the address bar.

### Safari

Safari cannot add custom search shortcuts on its own. You need a third-party Safari extension that supports keyword searches. Point it at the URL above.

### Why not just type the number?

Your browser only sends a bare number to your default search engine. You could make pr-jump your default, but then every search you type would go to GitHub's PR search, and searching would stop working whenever the server is not running. A two-letter shortcut avoids both problems.

## How it works

- On first start, pr-jump uses the GitHub GraphQL API to list every PR number, repository and creation date for your owners. It stores them in a SQLite file in `~/.local/share/pr-jump/` (or `$XDG_DATA_HOME/pr-jump/`).
- Every 2 minutes, it asks the GitHub search API for PRs opened since the newest one it has. Change the interval with `--refresh <seconds>`.
- Once a day, it rebuilds the whole index. This picks up repositories that were moved into your organisation and drops deleted ones. If a repository fails to load, its old entries stay until the next rebuild.
- If you ask for a number it does not have, it checks GitHub for new PRs before sending you to the search page. This takes a second or two, and happens at most once every 15 seconds. It never waits for a rebuild.

To rebuild the index straight away:

```sh
python3 pr_jump.py --owner my-org --rebuild
```

Use the same owners as the running service, because each set of owners has its own index file. The running server picks up the new index straight away.

## Privacy and security

- The index holds only repository names, PR numbers and dates. It does not store titles, descriptions or code. It still lists private repository names, so pr-jump makes its data folder readable only by your account.
- The server listens only on `127.0.0.1`.
- It answers only navigations you start yourself, such as typing in the address bar. It refuses requests from web pages, which browsers mark with a `Sec-Fetch-Site` header, so other sites cannot probe the index. It also refuses any host name other than `127.0.0.1` or `localhost`, which blocks DNS rebinding.

## Development

Run the tests with:

```sh
python3 -m unittest -v
```

The installer test runs only on macOS. It uses a stand-in for `launchctl`, so it never changes your real launch agents.

The macOS install has been tested. The Linux, Windows and browser instructions have not.
