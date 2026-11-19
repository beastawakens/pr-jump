"""Tests for pr_jump. Run with: python3 -m unittest -v"""
import http.client
import socket
import time
import os
import plistlib
import subprocess
import sys
import tempfile
import threading
import unittest
import urllib.error
from http.server import ThreadingHTTPServer
from unittest import mock

import pr_jump

HERE = os.path.dirname(os.path.abspath(__file__))


class IndexTest(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        pr_jump.db_path = os.path.join(self.dir.name, "test.db")
        pr_jump.owners = ["acme", "other"]
        pr_jump.last_miss_refresh = 0.0
        pr_jump.save([
            ("acme/ios", 42, "2023-06-30T14:19:09Z"),
            ("acme/ios-v12", 42, "2026-03-18T17:04:47Z"),
            ("acme/sdk-php", 42, "2026-11-17T17:01:12Z"),
            ("other/ios", 42, "2024-01-01T00:00:00Z"),
            ("acme/api", 5219, "2026-11-10T14:18:24Z"),
        ])

    def tearDown(self):
        self.dir.cleanup()


class ParseTest(unittest.TestCase):
    def test_queries(self):
        cases = {
            "5219": (5219, ""),
            "#5219": (5219, ""),
            " 5219 ": (5219, ""),
            "42 ios": (42, "ios"),
            "ios 42": (42, "ios"),
            "ios#42": (42, "ios"),
            "acme/ios#42": (42, "acme/ios"),
            "ios/42": (42, "ios"),
            "ACME/iOS 42": (42, "acme/ios"),
            "ios-v12 42": (42, "ios-v12"),
            "flaky tests": (None, "flaky tests"),
            "12345678901234567890123": (None, "12345678901234567890123"),
            "": (None, ""),
        }
        for query, expected in cases.items():
            with self.subTest(query=query):
                self.assertEqual(pr_jump.parse(query), expected)


class LookupTest(IndexTest):
    def test_newest_repo_wins_without_a_hint(self):
        self.assertEqual(pr_jump.lookup(42, ""), "acme/sdk-php")

    def test_exact_repo_name_beats_newer_partial_match(self):
        self.assertEqual(pr_jump.lookup(42, "ios"), "other/ios")

    def test_owner_and_repo_picks_that_repo(self):
        self.assertEqual(pr_jump.lookup(42, "acme/ios"), "acme/ios")

    def test_partial_hint(self):
        self.assertEqual(pr_jump.lookup(42, "v12"), "acme/ios-v12")

    def test_unknown_number_or_hint(self):
        self.assertIsNone(pr_jump.lookup(43, ""))
        self.assertIsNone(pr_jump.lookup(42, "android"))

    def test_rebuild_replaces_old_rows(self):
        pr_jump.save([("acme/ios", 7, "2020-01-01T00:00:00Z")], keep=[])
        self.assertIsNone(pr_jump.lookup(42, ""))
        self.assertEqual(pr_jump.lookup(7, ""), "acme/ios")


class DestinationTest(IndexTest):
    def test_known_pr(self):
        self.assertEqual(pr_jump.destination("5219"), "https://github.com/acme/api/pull/5219")

    def test_text_goes_to_github_search(self):
        url = pr_jump.destination("flaky tests")
        self.assertTrue(url.startswith("https://github.com/pulls?q="))
        self.assertIn("user%3Aacme%20user%3Aother%20flaky%20tests", url)

    def test_miss_refreshes_once_then_finds_new_pr(self):
        def refresh(allow_rebuild=True):
            pr_jump.save([("other/web", 9000, "2026-11-19T09:00:00Z")])
        with mock.patch.object(pr_jump, "refresh", side_effect=refresh) as fake:
            self.assertEqual(pr_jump.destination("9000"), "https://github.com/other/web/pull/9000")
            self.assertEqual(fake.call_count, 1)

    def test_misses_are_throttled(self):
        with mock.patch.object(pr_jump, "refresh") as fake:
            pr_jump.destination("9001")
            pr_jump.destination("9002")
        self.assertEqual(fake.call_count, 1)

    def test_miss_never_waits_for_the_background_thread(self):
        with pr_jump.lock, mock.patch.object(pr_jump, "refresh") as fake:
            started = time.time()
            self.assertIn("github.com/pulls?q=", pr_jump.destination("9004"))
        self.assertLess(time.time() - started, 0.5)
        fake.assert_not_called()

    def test_miss_never_rebuilds(self):
        with mock.patch.object(pr_jump, "refresh") as fake:
            pr_jump.destination("9005")
        fake.assert_called_once_with(allow_rebuild=False)

    def test_long_numbers_go_to_search(self):
        self.assertIn("github.com/pulls?q=", pr_jump.destination("12345678901234567890123"))

    def test_refresh_failure_still_redirects(self):
        with mock.patch.object(pr_jump, "refresh", side_effect=RuntimeError("offline")), \
                mock.patch.object(pr_jump, "log"):
            self.assertIn("github.com/pulls?q=", pr_jump.destination("9003"))


class RefreshTest(IndexTest):
    def setUp(self):
        super().setUp()
        db = pr_jump.connect()
        with db:
            db.execute("insert or replace into meta values ('rebuilt_at', ?)", (time.time(),))
        db.close()

    def item(self, repo, number, created):
        return {"repository_url": "https://api.github.com/repos/" + repo, "number": number, "created_at": created}

    def test_adds_new_prs_from_search(self):
        with mock.patch.object(pr_jump, "github", return_value={"items": [
                self.item("other/web", 77, "2026-11-19T08:00:00Z")]}) as fake:
            pr_jump.refresh()
        self.assertEqual(pr_jump.lookup(77, ""), "other/web")
        query = fake.call_args[0][0]
        self.assertIn("user%3Aacme+user%3Aother+is%3Apr", query)
        self.assertIn("created%3A%3E%3D2026-11-17T16%3A01%3A12Z", query)  # newest minus an hour

    def test_more_than_search_can_return_triggers_rebuild(self):
        page = {"items": [self.item("acme/api", n, "2026-11-19T08:00:00Z") for n in range(100)]}
        with mock.patch.object(pr_jump, "github", return_value=page), \
                mock.patch.object(pr_jump, "rebuild") as rebuild:
            pr_jump.refresh()
        rebuild.assert_called_once()

    def test_incomplete_search_results_are_not_saved(self):
        page = {"incomplete_results": True, "items": [self.item("other/web", 77, "2026-11-19T08:00:00Z")]}
        with mock.patch.object(pr_jump, "github", return_value=page), mock.patch.object(pr_jump, "log"):
            pr_jump.refresh()
        self.assertIsNone(pr_jump.lookup(77, ""))

    def test_day_old_index_is_rebuilt(self):
        db = pr_jump.connect()
        with db:
            db.execute("insert or replace into meta values ('rebuilt_at', ?)", (time.time() - 2 * 86400,))
        db.close()
        with mock.patch.object(pr_jump, "rebuild") as rebuild:
            pr_jump.refresh()
        rebuild.assert_called_once()

    def test_requests_never_rebuild(self):
        pr_jump.save([], keep=[])
        with mock.patch.object(pr_jump, "rebuild") as rebuild, mock.patch.object(pr_jump, "github") as github:
            pr_jump.refresh(allow_rebuild=False)
        rebuild.assert_not_called()
        github.assert_not_called()

    def test_empty_index_triggers_rebuild(self):
        pr_jump.save([], keep=[])
        with mock.patch.object(pr_jump, "rebuild") as rebuild:
            pr_jump.refresh()
        rebuild.assert_called_once()


class RebuildTest(IndexTest):
    def test_pages_through_repos_and_prs(self):
        def page(nodes, cursor=None):
            return {"nodes": nodes, "pageInfo": {"hasNextPage": cursor is not None, "endCursor": cursor}}

        def github(path, body):
            v = body["variables"]
            if "repositoryOwner" in body["query"]:
                repos = {"acme": [{"nameWithOwner": "acme/api"}], "other": [{"nameWithOwner": "other/web"}]}
                return {"data": {"repositoryOwner": {"repositories": page(repos[v["owner"]])}}}
            if v["name"] == "api" and v["after"] is None:
                prs = page([{"number": 1, "createdAt": "2020-01-01T00:00:00Z"}], cursor="next")
            elif v["name"] == "api":
                prs = page([{"number": 2, "createdAt": "2020-01-02T00:00:00Z"}])
            else:
                prs = page([{"number": 1, "createdAt": "2021-01-01T00:00:00Z"}])
            return {"data": {"repository": {"pullRequests": prs}}}

        with mock.patch.object(pr_jump, "github", side_effect=github), mock.patch.object(pr_jump, "log"):
            pr_jump.rebuild()
        self.assertEqual(pr_jump.lookup(1, ""), "other/web")
        self.assertEqual(pr_jump.lookup(2, ""), "acme/api")
        self.assertIsNone(pr_jump.lookup(42, ""))

    def test_failures_keep_old_rows_and_skip_missing_owners(self):
        pr_jump.owners = ["acme", "ghost"]

        def github(path, body):
            v = body["variables"]
            if "repositoryOwner" in body["query"]:
                if v["owner"] == "ghost":
                    return {"data": {"repositoryOwner": None}}
                nodes = [{"nameWithOwner": "acme/ios"}, {"nameWithOwner": "acme/api"}]
                return {"data": {"repositoryOwner": {"repositories": {
                    "nodes": nodes, "pageInfo": {"hasNextPage": False, "endCursor": None}}}}}
            if v["name"] == "ios":
                return {"data": {"repository": None}, "errors": [{"type": "NOT_FOUND"}]}
            return {"data": {"repository": {"pullRequests": {
                "nodes": [{"number": 1, "createdAt": "2020-01-01T00:00:00Z"}],
                "pageInfo": {"hasNextPage": False, "endCursor": None}}}}}

        with mock.patch.object(pr_jump, "github", side_effect=github), mock.patch.object(pr_jump, "log") as log:
            pr_jump.rebuild()
        self.assertEqual(pr_jump.lookup(42, "acme/ios"), "acme/ios")  # kept: its fetch failed
        self.assertIsNone(pr_jump.lookup(5219, ""))  # dropped: acme/api no longer has it
        self.assertEqual(pr_jump.lookup(1, ""), "acme/api")
        self.assertIn("ghost", " ".join(str(c) for c in log.call_args_list))

    def test_no_repositories_at_all_raises_and_keeps_index(self):
        with mock.patch.object(pr_jump, "github", return_value={"data": {"repositoryOwner": None}}), \
                mock.patch.object(pr_jump, "log"):
            with self.assertRaises(RuntimeError):
                pr_jump.rebuild()
        self.assertEqual(pr_jump.lookup(5219, ""), "acme/api")

    def test_graphql_errors_raise(self):
        with mock.patch.object(pr_jump, "github", return_value={"errors": [{"message": "nope"}]}):
            with self.assertRaises(RuntimeError):
                list(pr_jump.nodes(pr_jump.REPOS_QUERY, ["repositoryOwner"], owner="acme"))


class GithubRetryTest(unittest.TestCase):
    def setUp(self):
        patches = [mock.patch.object(pr_jump, "token", return_value="t"), mock.patch.object(pr_jump.time, "sleep")]
        for patch in patches:
            patch.start()
            self.addCleanup(patch.stop)

    def response(self):
        response = mock.MagicMock()
        response.__enter__.return_value.read.return_value = b'{"ok": true}'
        return response

    def http_error(self, code, headers=None):
        return urllib.error.HTTPError("u", code, "x", headers or {}, None)

    def test_retries_timeouts_and_server_errors(self):
        effects = [urllib.error.URLError("handshake timed out"), self.http_error(502), self.response()]
        with mock.patch.object(pr_jump.urllib.request, "urlopen", side_effect=effects) as urlopen:
            self.assertEqual(pr_jump.github("/x"), {"ok": True})
        self.assertEqual(urlopen.call_count, 3)

    def test_retries_read_timeouts_and_dropped_connections(self):
        effects = [socket.timeout("read timed out"), http.client.RemoteDisconnected("closed"), self.response()]
        with mock.patch.object(pr_jump.urllib.request, "urlopen", side_effect=effects) as urlopen:
            self.assertEqual(pr_jump.github("/x"), {"ok": True})
        self.assertEqual(urlopen.call_count, 3)

    def test_honours_retry_after(self):
        effects = [self.http_error(403, {"Retry-After": "7"}), self.response()]
        with mock.patch.object(pr_jump.urllib.request, "urlopen", side_effect=effects):
            pr_jump.github("/x")
        pr_jump.time.sleep.assert_called_once_with(7)

    def test_does_not_retry_auth_errors(self):
        with mock.patch.object(pr_jump.urllib.request, "urlopen", side_effect=[self.http_error(401)]):
            with self.assertRaises(urllib.error.HTTPError):
                pr_jump.github("/x")

    def test_gives_up_after_four_attempts(self):
        with mock.patch.object(pr_jump.urllib.request, "urlopen",
                               side_effect=urllib.error.URLError("down")) as urlopen:
            with self.assertRaises(urllib.error.URLError):
                pr_jump.github("/x")
        self.assertEqual(urlopen.call_count, 4)


class ServerTest(IndexTest):
    def setUp(self):
        super().setUp()
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), pr_jump.Handler)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.port = self.server.server_address[1]

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        super().tearDown()

    def get(self, path, host, **headers):
        connection = http.client.HTTPConnection("127.0.0.1", self.port)
        connection.request("GET", path, headers=dict(headers, Host=host))
        response = connection.getresponse()
        connection.close()
        return response.status, response.getheader("Location")

    def test_redirects_to_pr(self):
        for host in (f"127.0.0.1:{self.port}", f"localhost:{self.port}"):
            with self.subTest(host=host):
                self.assertEqual(self.get("/?q=%23" + "5219", host),
                                 (302, "https://github.com/acme/api/pull/5219"))

    def test_refuses_other_host_names(self):
        self.assertEqual(self.get("/?q=5219", "attacker.example:7777")[0], 403)

    def test_answers_navigations_typed_by_the_user(self):
        host = f"127.0.0.1:{self.port}"
        self.assertEqual(self.get("/?q=5219", host, **{"Sec-Fetch-Site": "none"})[0], 302)

    def test_refuses_requests_from_other_sites(self):
        host = f"127.0.0.1:{self.port}"
        for site in ("cross-site", "same-site", "same-origin"):
            with self.subTest(site=site):
                self.assertEqual(self.get("/?q=5219", host, **{"Sec-Fetch-Site": site})[0], 403)


@unittest.skipUnless(sys.platform == "darwin", "macOS installer")
class InstallerTest(unittest.TestCase):
    def test_writes_a_valid_launch_agent_and_uninstalls(self):
        with tempfile.TemporaryDirectory() as home:
            bin_dir = os.path.join(home, "bin")
            os.mkdir(bin_dir)
            calls = os.path.join(home, "calls")
            # Stand-ins for launchctl and gh. With a dummy token, the owner check gets a 401
            # (or no network), which it logs and ignores.
            for name, body in (("launchctl", f'echo "$*" >> "{calls}"'), ("gh", "echo dummy-token")):
                stub = os.path.join(bin_dir, name)
                with open(stub, "w") as f:
                    f.write(f"#!/bin/sh\n{body}\n")
                os.chmod(stub, 0o755)
            env = dict(os.environ, HOME=home, PATH=bin_dir + os.pathsep + os.environ["PATH"])
            env.pop("XDG_DATA_HOME", None)
            installer = os.path.join(HERE, "install-macos.sh")
            data = os.path.join(home, ".local/share/pr-jump")

            usage = subprocess.run([installer], env=env, capture_output=True)
            self.assertEqual(usage.returncode, 2)
            self.assertFalse(os.path.exists(calls), "printing usage must not stop the service")

            subprocess.run([installer, "--owner", "acme", "--port", "7778"], env=env, check=True,
                           capture_output=True)
            agent = os.path.join(home, "Library/LaunchAgents/local.pr-jump.plist")
            with open(agent, "rb") as f:
                plist = plistlib.load(f)
            self.assertEqual(plist["ProgramArguments"][1:],
                             [os.path.join(HERE, "pr_jump.py"), "--owner", "acme", "--port", "7778",
                              "--data-dir", data])
            self.assertEqual(os.stat(data).st_mode & 0o777, 0o700)
            self.assertTrue(plist["KeepAlive"])

            subprocess.run([installer, "--uninstall"], env=env, check=True, capture_output=True)
            self.assertFalse(os.path.exists(agent))
            with open(calls) as f:
                self.assertIn("bootstrap", f.read())


if __name__ == "__main__":
    unittest.main()
