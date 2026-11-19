#!/usr/bin/env python3
"""pr-jump: type a pull request number in the address bar and land on the PR.

Keeps a local SQLite index of every pull request owned by the GitHub users or
organisations you name, and redirects http://127.0.0.1:7777/?q=<query> to the
newest PR with that number. Standard library only, Python 3.9 or later.

  pr_jump.py --owner my-org [--owner another] [--port 7777]
  pr_jump.py --owner my-org --rebuild    refetch every PR, then exit
  pr_jump.py --owner my-org --validate   check the owners exist, then exit
"""
import argparse
import datetime
import http.client
import json
import os
import re
import shutil
import sqlite3
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from concurrent.futures import ThreadPoolExecutor
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

# Up to 9 digits, so pasted IDs fall through to text search instead of overflowing SQLite.
NUMBER = re.compile(r"(?:^|[\s#/])(\d{1,9})(?=\s|$)")
MISS_REFRESH_SECONDS = 15
REBUILD_SECONDS = 24 * 60 * 60
REPOS_QUERY = """query($owner: String!, $after: String) {
  repositoryOwner(login: $owner) { repositories(first: 100, after: $after, ownerAffiliations: OWNER) {
    nodes { nameWithOwner } pageInfo { hasNextPage endCursor } } } }"""
PRS_QUERY = """query($owner: String!, $name: String!, $after: String) {
  repository(owner: $owner, name: $name) { pullRequests(first: 100, after: $after) {
    nodes { number createdAt } pageInfo { hasNextPage endCursor } } } }"""

owners = []
db_path = ""
lock = threading.Lock()
last_miss_refresh = 0.0


def token():
    if os.environ.get("GITHUB_TOKEN"):
        return os.environ["GITHUB_TOKEN"]
    gh = shutil.which("gh")
    if not gh:
        raise RuntimeError("Set GITHUB_TOKEN or install the GitHub CLI (gh) and run 'gh auth login'")
    return subprocess.run([gh, "auth", "token"], capture_output=True, text=True, check=True).stdout.strip()


def github(path, body=None, attempts=4):
    """Call the GitHub API, retrying network errors, 5xx and rate-limit responses."""
    for attempt in range(attempts):
        request = urllib.request.Request(
            "https://api.github.com" + path,
            data=None if body is None else json.dumps(body).encode(),
            headers={"Authorization": "Bearer " + token(), "Accept": "application/vnd.github+json"},
        )
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            wait = error.headers.get("Retry-After")
            if attempt == attempts - 1 or not (error.code >= 500 or error.code == 429 or wait):
                raise
            delay = int(wait) if wait and wait.isdigit() else 2 ** attempt
        except (OSError, http.client.HTTPException):  # Timeouts, resets and truncated replies.
            if attempt == attempts - 1:
                raise
            delay = 2 ** attempt
        time.sleep(delay)


def nodes(query, path, **variables):
    after = None
    while True:
        result = github("/graphql", {"query": query, "variables": dict(variables, after=after)})
        if result.get("errors"):
            raise RuntimeError(result["errors"])
        connection = result["data"]
        for key in path:
            connection = connection[key]
            if connection is None:
                raise LookupError(f"{key} not found: {variables}")
        yield from connection["nodes"]
        if not connection["pageInfo"]["hasNextPage"]:
            return
        after = connection["pageInfo"]["endCursor"]


def connect(path=None):
    db = sqlite3.connect(path or db_path)
    db.execute("pragma journal_mode=wal")
    db.execute("create table if not exists prs (repo text, number integer, created_at text, primary key (repo, number))")
    db.execute("create index if not exists prs_number on prs (number)")
    db.execute("create table if not exists meta (key text primary key, value real)")
    return db


def save(rows, path=None, keep=None):
    """Add rows. With keep, first delete every row except those of repos (or 'owner/' prefixes) in keep."""
    db = connect(path)
    with db:
        if keep is not None:
            clauses = ["repo like ?" if k.endswith("/") else "repo = ?" for k in keep]
            params = [k + "%" if k.endswith("/") else k for k in keep]
            db.execute("delete from prs" + (f" where not ({' or '.join(clauses)})" if keep else ""), params)
            db.execute("insert or replace into meta values ('rebuilt_at', ?)", (time.time(),))
        db.executemany("insert or replace into prs values (?, ?, ?)", rows)
    db.close()


def rebuild():
    repos, failed = [], []
    for owner in owners:
        try:
            repos += [r["nameWithOwner"] for r in nodes(REPOS_QUERY, ["repositoryOwner", "repositories"], owner=owner)]
        except Exception as error:
            log(f"could not list repositories for {owner}: {error}")
            failed.append(owner + "/")
    if not repos:
        raise RuntimeError("no repositories found for " + ", ".join(owners))

    def fetch(repo):
        owner, name = repo.split("/")
        try:
            return [(repo, p["number"], p["createdAt"])
                    for p in nodes(PRS_QUERY, ["repository", "pullRequests"], owner=owner, name=name)]
        except Exception as error:
            log(f"could not index {repo}: {error}")
            failed.append(repo)
            return []

    with ThreadPoolExecutor(8) as pool:
        rows = [row for batch in pool.map(fetch, repos) for row in batch]
    save(rows, keep=failed)  # Keep the old rows of anything that failed this time.
    log(f"indexed {len(rows)} PRs in {len(repos)} repos" + (f", {len(failed)} failed" if failed else ""))


def refresh(allow_rebuild=True):
    """Add PRs opened since the newest one indexed. Rebuild when empty or a day old, if allowed."""
    db = connect()
    newest = db.execute("select max(created_at) from prs").fetchone()[0]
    rebuilt_at = (db.execute("select value from meta where key = 'rebuilt_at'").fetchone() or [0])[0]
    db.close()
    if allow_rebuild and (not newest or time.time() - rebuilt_at > REBUILD_SECONDS):
        return rebuild()
    if not newest:
        return
    # Overlap by an hour because search indexes new PRs with a delay.
    since = datetime.datetime.strptime(newest, "%Y-%m-%dT%H:%M:%SZ") - datetime.timedelta(hours=1)
    query = " ".join(f"user:{o}" for o in owners) + f" is:pr created:>={since:%Y-%m-%dT%H:%M:%SZ}"
    rows = []
    for page in range(1, 11):
        result = github("/search/issues?" + urllib.parse.urlencode(
            {"q": query, "sort": "created", "order": "asc", "per_page": 100, "page": page}))
        if result.get("incomplete_results"):
            return log("search returned incomplete results; trying again next time")
        items = result["items"]
        rows += [("/".join(i["repository_url"].split("/")[-2:]), i["number"], i["created_at"]) for i in items]
        if len(items) < 100:
            break
    else:
        return rebuild() if allow_rebuild else None  # Search stops at 1,000 results.
    save(rows)


def parse(query):
    """Split a query into (PR number, repo hint), or (None, query) without a number."""
    match = NUMBER.search(query)
    if not match:
        return None, query.strip()
    hint = (query[:match.start()] + " " + query[match.end():]).strip(" #/").lower()
    return int(match.group(1)), hint


def lookup(number, hint, path=None):
    """Newest repo with this PR number, preferring an exact owner/repo, then repo, then partial match."""
    db = connect(path)
    repos = [r[0] for r in db.execute("select repo from prs where number = ? order by created_at desc", (number,))]
    db.close()
    if hint:
        repos = ([r for r in repos if r.lower() == hint]
                 or [r for r in repos if r.split("/")[1].lower() == hint]
                 or [r for r in repos if hint in r.lower()])
    return repos[0] if repos else None


def destination(query):
    global last_miss_refresh
    number, hint = parse(query)
    if number is not None:
        repo = lookup(number, hint)
        # Maybe it opened since the last refresh. Never wait for the background thread or rebuild here.
        if repo is None and time.time() - last_miss_refresh > MISS_REFRESH_SECONDS and lock.acquire(blocking=False):
            last_miss_refresh = time.time()
            try:
                refresh(allow_rebuild=False)
            except Exception as error:
                log(f"refresh failed: {error}")
            finally:
                lock.release()
            repo = lookup(number, hint)
        if repo:
            return f"https://github.com/{repo}/pull/{number}"
    search = f"is:pr {' '.join('user:' + o for o in owners)} {query.strip()} sort:created-desc"
    return "https://github.com/pulls?q=" + urllib.parse.quote(search)


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        # Answer only navigations the user started, from localhost: other sites' pages could
        # otherwise probe the index (cross-site requests) or read it (DNS rebinding).
        host = self.headers.get("Host", "").rsplit(":", 1)[0]
        if host not in ("127.0.0.1", "localhost") or self.headers.get("Sec-Fetch-Site", "none") != "none":
            self.send_error(403)
            return
        params = urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query)
        self.send_response(302)
        self.send_header("Location", destination(params.get("q", [""])[0]))
        self.end_headers()

    def log_message(self, *args):
        pass


def log(message):
    print(f"{datetime.datetime.now():%Y-%m-%d %H:%M:%S} {message}", file=sys.stderr, flush=True)


def background(interval):
    while True:
        with lock:
            try:
                refresh()
            except Exception as error:
                log(f"refresh failed: {error}")
        time.sleep(interval)


def missing_owners():
    """Owners GitHub says do not exist. Network errors are ignored so the service still starts offline."""
    missing = []
    for owner in owners:
        try:
            github("/users/" + urllib.parse.quote(owner))
        except urllib.error.HTTPError as error:
            if error.code == 404:
                missing.append(owner)
        except Exception as error:
            log(f"could not check owner {owner}: {error}")
    return missing


def main():
    global owners, db_path
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--owner", action="append", default=[], help="GitHub user or organisation; repeat for more")
    parser.add_argument("--port", type=int, default=7777)
    parser.add_argument("--refresh", type=int, default=120, help="seconds between checks for new PRs")
    parser.add_argument("--data-dir", default=os.path.join(
        os.environ.get("XDG_DATA_HOME", os.path.expanduser("~/.local/share")), "pr-jump"))
    parser.add_argument("--rebuild", action="store_true", help="refetch every PR, then exit")
    parser.add_argument("--validate", action="store_true", help="check the owners exist, then exit")
    args = parser.parse_args()
    owners = sorted({o.strip().lower() for value in args.owner for o in value.split(",") if o.strip()})
    if not owners:
        parser.error("name at least one --owner")
    missing = missing_owners()
    if missing:
        parser.error("no GitHub user or organisation called " + ", ".join(missing))
    if args.validate:
        return
    # The index lists private repository names, so keep it to this account.
    os.makedirs(args.data_dir, mode=0o700, exist_ok=True)
    os.chmod(args.data_dir, 0o700)
    db_path = os.path.join(args.data_dir, "+".join(owners) + ".db")
    if args.rebuild:
        return rebuild()
    threading.Thread(target=background, args=(args.refresh,), daemon=True).start()
    log(f"serving http://127.0.0.1:{args.port}/?q=%s for {', '.join(owners)}")
    ThreadingHTTPServer(("127.0.0.1", args.port), Handler).serve_forever()


if __name__ == "__main__":
    main()
