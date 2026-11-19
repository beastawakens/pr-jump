package main

import (
	"encoding/json"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHub answers API calls with the handler a test gives it and counts them by path.
type fakeGitHub struct {
	mu    sync.Mutex
	calls map[string]int
}

func (f *fakeGitHub) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[path]
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// newTestApp returns an app for owners acme and other, indexed with a few PRs and freshly
// rebuilt. With a nil handler, any call to GitHub fails the test.
func newTestApp(t *testing.T, handler http.HandlerFunc) (*app, *fakeGitHub) {
	t.Helper()
	fake := &fakeGitHub{calls: map[string]int{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		fake.calls[r.URL.Path]++
		fake.mu.Unlock()
		if handler == nil {
			t.Errorf("unexpected GitHub call: %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", http.StatusTeapot)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)

	a := newApp([]string{"acme", "other"}, log.New(io.Discard, "", 0))
	a.api = server.URL
	a.token = func() (string, error) { return "test-token", nil }
	a.sleep = func(time.Duration) {}
	db, err := openIndex(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	a.db = db
	err = a.save([]row{
		{"acme/ios", 42, "2023-06-30T14:19:09Z"},
		{"acme/ios-v12", 42, "2026-03-18T17:04:47Z"},
		{"acme/sdk-php", 42, "2026-11-17T17:01:12Z"},
		{"other/ios", 42, "2024-01-01T00:00:00Z"},
		{"acme/api", 5219, "2026-11-10T14:18:24Z"},
	}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	return a, fake
}

func mustLookup(t *testing.T, a *app, number int, hint string) string {
	t.Helper()
	repo, err := a.lookup(number, hint)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

func rebuiltAt(t *testing.T, a *app) float64 {
	t.Helper()
	var value float64
	if err := a.db.QueryRow("select value from meta where key = 'rebuilt_at'").Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func setRebuiltAt(t *testing.T, a *app, at time.Time) {
	t.Helper()
	if _, err := a.db.Exec("insert or replace into meta values ('rebuilt_at', ?)", epoch(at)); err != nil {
		t.Fatal(err)
	}
}

func emptyIndex(t *testing.T, a *app) {
	t.Helper()
	if err := a.save(nil, true, nil); err != nil {
		t.Fatal(err)
	}
}

func TestParse(t *testing.T) {
	cases := []struct {
		query  string
		number int
		hint   string
		ok     bool
	}{
		{"5219", 5219, "", true},
		{"#5219", 5219, "", true},
		{" 5219 ", 5219, "", true},
		{"42 ios", 42, "ios", true},
		{"ios 42", 42, "ios", true},
		{"ios#42", 42, "ios", true},
		{"ios/42", 42, "ios", true},
		{"acme/ios#42", 42, "acme/ios", true},
		{"ACME/iOS 42", 42, "acme/ios", true},
		{"ios-v12 42", 42, "ios-v12", true},
		{"flaky tests", 0, "flaky tests", false},
		{"12345678901234567890123", 0, "12345678901234567890123", false},
		{"", 0, "", false},
	}
	for _, c := range cases {
		number, hint, ok := parse(c.query)
		if number != c.number || hint != c.hint || ok != c.ok {
			t.Errorf("parse(%q) = %d, %q, %v; want %d, %q, %v", c.query, number, hint, ok, c.number, c.hint, c.ok)
		}
	}
}

func TestLookup(t *testing.T) {
	a, _ := newTestApp(t, nil)
	cases := []struct {
		number int
		hint   string
		want   string
	}{
		{42, "", "acme/sdk-php"},     // newest wins without a hint
		{42, "ios", "other/ios"},     // exact repository name beats a newer partial match
		{42, "acme/ios", "acme/ios"}, // owner and repository picks that one
		{42, "v12", "acme/ios-v12"},  // partial hint
		{43, "", ""},                 // unknown number
		{42, "android", ""},          // unknown hint
	}
	for _, c := range cases {
		if got := mustLookup(t, a, c.number, c.hint); got != c.want {
			t.Errorf("lookup(%d, %q) = %q; want %q", c.number, c.hint, got, c.want)
		}
	}
}

func TestRebuildSaveReplacesOldRows(t *testing.T) {
	a, _ := newTestApp(t, nil)
	if err := a.save([]row{{"acme/ios", 7, "2020-01-01T00:00:00Z"}}, true, nil); err != nil {
		t.Fatal(err)
	}
	if got := mustLookup(t, a, 42, ""); got != "" {
		t.Errorf("old row survived: %q", got)
	}
	if got := mustLookup(t, a, 7, ""); got != "acme/ios" {
		t.Errorf("new row missing: %q", got)
	}
}

func searchItems(items ...[3]any) map[string]any {
	var out []map[string]any
	for _, i := range items {
		out = append(out, map[string]any{
			"repository_url": "https://api.github.com/repos/" + i[0].(string), "number": i[1], "created_at": i[2]})
	}
	return map[string]any{"incomplete_results": false, "items": out}
}

func TestDestination(t *testing.T) {
	t.Run("known PR", func(t *testing.T) {
		a, _ := newTestApp(t, nil)
		if got := a.destination("5219"); got != "https://github.com/acme/api/pull/5219" {
			t.Error(got)
		}
	})
	t.Run("text goes to GitHub search", func(t *testing.T) {
		a, _ := newTestApp(t, nil)
		got := a.destination("flaky tests")
		if !strings.HasPrefix(got, "https://github.com/pulls?q=") || !strings.Contains(got, "user%3Aacme+user%3Aother+flaky+tests") {
			t.Error(got)
		}
	})
	t.Run("long numbers go to search", func(t *testing.T) {
		a, _ := newTestApp(t, nil)
		if got := a.destination("12345678901234567890123"); !strings.Contains(got, "github.com/pulls?q=") {
			t.Error(got)
		}
	})
	t.Run("a miss refreshes once and finds the new PR", func(t *testing.T) {
		a, fake := newTestApp(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, searchItems([3]any{"other/web", 9000, "2026-11-19T09:00:00Z"}))
		})
		if got := a.destination("9000"); got != "https://github.com/other/web/pull/9000" {
			t.Error(got)
		}
		if n := fake.count("/search/issues"); n != 1 {
			t.Errorf("%d searches; want 1", n)
		}
	})
	t.Run("misses are throttled", func(t *testing.T) {
		a, fake := newTestApp(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, searchItems()) })
		a.destination("9001")
		a.destination("9002")
		if n := fake.count("/search/issues"); n != 1 {
			t.Errorf("%d searches; want 1", n)
		}
	})
	t.Run("a miss never waits for the background refresh", func(t *testing.T) {
		a, _ := newTestApp(t, nil)
		a.mu.Lock()
		defer a.mu.Unlock()
		started := time.Now()
		if got := a.destination("9004"); !strings.Contains(got, "github.com/pulls?q=") {
			t.Error(got)
		}
		if time.Since(started) > 500*time.Millisecond {
			t.Error("destination waited for the lock")
		}
	})
	t.Run("a miss never rebuilds", func(t *testing.T) {
		a, _ := newTestApp(t, nil) // Any GitHub call fails the test.
		emptyIndex(t, a)
		a.destination("9005")
	})
	t.Run("a failed refresh still redirects", func(t *testing.T) {
		a, _ := newTestApp(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
		})
		if got := a.destination("9003"); !strings.Contains(got, "github.com/pulls?q=") {
			t.Error(got)
		}
	})
}

func TestRefresh(t *testing.T) {
	t.Run("adds new PRs from search", func(t *testing.T) {
		var query string
		a, _ := newTestApp(t, func(w http.ResponseWriter, r *http.Request) {
			query = r.URL.Query().Get("q")
			writeJSON(w, searchItems([3]any{"other/web", 77, "2026-11-19T08:00:00Z"}))
		})
		if err := a.refresh(true); err != nil {
			t.Fatal(err)
		}
		if got := mustLookup(t, a, 77, ""); got != "other/web" {
			t.Error(got)
		}
		// The newest indexed PR minus an hour.
		if want := "user:acme user:other is:pr created:>=2026-11-17T16:01:12Z"; query != want {
			t.Errorf("query %q; want %q", query, want)
		}
	})
	t.Run("incomplete search results are not saved", func(t *testing.T) {
		a, _ := newTestApp(t, func(w http.ResponseWriter, r *http.Request) {
			result := searchItems([3]any{"other/web", 77, "2026-11-19T08:00:00Z"})
			result["incomplete_results"] = true
			writeJSON(w, result)
		})
		if err := a.refresh(true); err != nil {
			t.Fatal(err)
		}
		if got := mustLookup(t, a, 77, ""); got != "" {
			t.Error(got)
		}
	})
	rebuilds := func(t *testing.T, prepare func(*app), allow bool, want bool) {
		t.Helper()
		a, fake := newTestApp(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/graphql" {
				writeJSON(w, map[string]any{"data": map[string]any{"repositoryOwner": nil}})
				return
			}
			var items [][3]any
			for n := 0; n < searchPageSize; n++ {
				items = append(items, [3]any{"acme/api", n, "2026-11-19T08:00:00Z"})
			}
			writeJSON(w, searchItems(items...))
		})
		prepare(a)
		_ = a.refresh(allow)
		if got := fake.count("/graphql") > 0; got != want {
			t.Errorf("rebuilt: %v; want %v", got, want)
		}
	}
	t.Run("an empty index is rebuilt", func(t *testing.T) {
		rebuilds(t, func(a *app) { emptyIndex(t, a) }, true, true)
	})
	t.Run("a day-old index is rebuilt", func(t *testing.T) {
		rebuilds(t, func(a *app) { setRebuiltAt(t, a, time.Now().Add(-48*time.Hour)) }, true, true)
	})
	t.Run("more new PRs than search can list means a rebuild", func(t *testing.T) {
		rebuilds(t, func(*app) {}, true, true)
	})
	t.Run("requests never rebuild", func(t *testing.T) {
		rebuilds(t, func(*app) {}, false, false)
		rebuilds(t, func(a *app) { emptyIndex(t, a) }, false, false)
	})
}

// graphQLHandler serves repository and PR listings. prs maps owner/name to PR pages; a nil
// page list makes that repository fail.
func graphQLHandler(t *testing.T, repos map[string][]string, prs map[string][][]map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		v := body.Variables
		connection := func(nodes any, page, pages int) map[string]any {
			return map[string]any{"nodes": nodes, "pageInfo": map[string]any{
				"hasNextPage": page < pages-1, "endCursor": map[bool]any{true: "p1", false: nil}[page < pages-1]}}
		}
		if strings.Contains(body.Query, "repositoryOwner") {
			names, ok := repos[v["owner"].(string)]
			if !ok {
				writeJSON(w, map[string]any{"data": map[string]any{"repositoryOwner": nil}})
				return
			}
			var nodes []map[string]any
			for _, n := range names {
				nodes = append(nodes, map[string]any{"nameWithOwner": n})
			}
			writeJSON(w, map[string]any{"data": map[string]any{"repositoryOwner": map[string]any{"repositories": connection(nodes, 0, 1)}}})
			return
		}
		pages := prs[v["owner"].(string)+"/"+v["name"].(string)]
		if pages == nil {
			writeJSON(w, map[string]any{"data": map[string]any{"repository": nil}, "errors": []any{map[string]any{"type": "NOT_FOUND"}}})
			return
		}
		page := 0
		if v["after"] == "p1" {
			page = 1
		}
		writeJSON(w, map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequests": connection(pages[page], page, len(pages))}}})
	}
}

func pr(number int, created string) map[string]any {
	return map[string]any{"number": number, "createdAt": created}
}

func TestRebuild(t *testing.T) {
	t.Run("pages through repositories and PRs", func(t *testing.T) {
		a, _ := newTestApp(t, graphQLHandler(t,
			map[string][]string{"acme": {"acme/api"}, "other": {"other/web"}},
			map[string][][]map[string]any{
				"acme/api":  {{pr(1, "2020-01-01T00:00:00Z")}, {pr(2, "2020-01-02T00:00:00Z")}},
				"other/web": {{pr(1, "2021-01-01T00:00:00Z")}},
			}))
		if err := a.rebuild(); err != nil {
			t.Fatal(err)
		}
		for number, want := range map[int]string{1: "other/web", 2: "acme/api", 42: ""} {
			if got := mustLookup(t, a, number, ""); got != want {
				t.Errorf("#%d in %q; want %q", number, got, want)
			}
		}
		if math.Abs(rebuiltAt(t, a)-epoch(time.Now())) > 5 {
			t.Error("rebuilt_at not set to now")
		}
	})
	t.Run("failures keep old rows, skip missing owners and retry soon", func(t *testing.T) {
		a, _ := newTestApp(t, graphQLHandler(t,
			map[string][]string{"acme": {"acme/ios", "acme/api"}},
			map[string][][]map[string]any{"acme/api": {{pr(1, "2020-01-01T00:00:00Z")}}}))
		a.owners = []string{"acme", "ghost"}
		if err := a.rebuild(); err != nil {
			t.Fatal(err)
		}
		if got := mustLookup(t, a, 42, "acme/ios"); got != "acme/ios" {
			t.Errorf("acme/ios lost its rows after failing: %q", got)
		}
		if got := mustLookup(t, a, 5219, ""); got != "" {
			t.Errorf("acme/api kept a PR it no longer has: %q", got)
		}
		if got := mustLookup(t, a, 1, ""); got != "acme/api" {
			t.Error(got)
		}
		want := epoch(time.Now().Add(retryFailedAfter - rebuildEvery))
		if math.Abs(rebuiltAt(t, a)-want) > 5 {
			t.Error("a rebuild with failures should make the next one due in ten minutes")
		}
	})
	t.Run("no repositories at all is an error and keeps the index", func(t *testing.T) {
		a, _ := newTestApp(t, graphQLHandler(t, nil, nil))
		if err := a.rebuild(); err == nil {
			t.Error("want an error")
		}
		if got := mustLookup(t, a, 5219, ""); got != "acme/api" {
			t.Error(got)
		}
	})
	t.Run("GraphQL errors are errors", func(t *testing.T) {
		a, _ := newTestApp(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"errors": []any{map[string]any{"message": "nope"}}})
		})
		if _, err := pages[struct{}](a, reposQuery, map[string]any{"owner": "acme"}, "repositoryOwner"); err == nil {
			t.Error("want an error")
		}
	})
}

func TestCallRetries(t *testing.T) {
	// respond serves the given statuses in turn; 0 drops the connection, and the last repeats.
	respond := func(t *testing.T, statuses []int, headers map[string]string) (*app, *fakeGitHub, *[]time.Duration) {
		var mu sync.Mutex
		n := 0
		a, fake := newTestApp(t, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			status := statuses[min(n, len(statuses)-1)]
			n++
			mu.Unlock()
			switch status {
			case 0:
				conn, _, _ := w.(http.Hijacker).Hijack()
				conn.Close()
			case 200:
				writeJSON(w, map[string]any{"ok": true})
			default:
				for k, v := range headers {
					w.Header().Set(k, v)
				}
				http.Error(w, "nope", status)
			}
		})
		var slept []time.Duration
		a.sleep = func(d time.Duration) { slept = append(slept, d) }
		return a, fake, &slept
	}
	call := func(a *app) error {
		var out struct{ OK bool }
		return a.call(http.MethodGet, "/x", nil, &out)
	}
	t.Run("retries dropped connections and server errors", func(t *testing.T) {
		a, fake, _ := respond(t, []int{0, 502, 200}, nil)
		if err := call(a); err != nil {
			t.Fatal(err)
		}
		if n := fake.count("/x"); n != 3 {
			t.Errorf("%d calls; want 3", n)
		}
	})
	t.Run("retries GraphQL timeouts", func(t *testing.T) {
		a, fake, _ := respond(t, []int{499, 200}, nil)
		if err := call(a); err != nil {
			t.Fatal(err)
		}
		if n := fake.count("/x"); n != 2 {
			t.Errorf("%d calls; want 2", n)
		}
	})
	t.Run("honours Retry-After", func(t *testing.T) {
		a, _, slept := respond(t, []int{403, 200}, map[string]string{"Retry-After": "7"})
		if err := call(a); err != nil {
			t.Fatal(err)
		}
		if len(*slept) != 1 || (*slept)[0] != 7*time.Second {
			t.Errorf("slept %v; want [7s]", *slept)
		}
	})
	t.Run("does not retry auth errors", func(t *testing.T) {
		a, fake, _ := respond(t, []int{401}, nil)
		if err := call(a); err == nil {
			t.Error("want an error")
		}
		if n := fake.count("/x"); n != 1 {
			t.Errorf("%d calls; want 1", n)
		}
	})
	t.Run("gives up after four attempts", func(t *testing.T) {
		a, fake, _ := respond(t, []int{0}, nil)
		if err := call(a); err == nil {
			t.Error("want an error")
		}
		if n := fake.count("/x"); n != apiAttempts {
			t.Errorf("%d calls; want %d", n, apiAttempts)
		}
	})
}

func TestMissingOwners(t *testing.T) {
	a, _ := newTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/acme":
			writeJSON(w, map[string]any{"login": "acme"})
		case "/users/ghost":
			http.Error(w, "not found", http.StatusNotFound)
		default:
			http.Error(w, "down", http.StatusBadGateway) // Logged and ignored.
		}
	})
	a.owners = []string{"acme", "ghost", "flaky"}
	if got, err := a.missingOwners(false); err != nil || len(got) != 1 || got[0] != "ghost" {
		t.Errorf("missing %v, %v; want [ghost]", got, err)
	}
	if _, err := a.missingOwners(true); err == nil || !strings.Contains(err.Error(), "flaky") {
		t.Errorf("strict check: %v; want an error about flaky", err)
	}
}

func TestServer(t *testing.T) {
	a, _ := newTestApp(t, nil)
	server := httptest.NewServer(a)
	t.Cleanup(server.Close)
	port := server.URL[strings.LastIndex(server.URL, ":")+1:]
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(host, site string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, server.URL+"/?q=%235219", nil)
		req.Host = host
		if site != "" {
			req.Header.Set("Sec-Fetch-Site", site)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode, resp.Header.Get("Location")
	}
	for _, host := range []string{"127.0.0.1:" + port, "localhost:" + port} {
		for _, site := range []string{"", "none"} {
			if status, location := get(host, site); status != http.StatusFound || location != "https://github.com/acme/api/pull/5219" {
				t.Errorf("host %s, Sec-Fetch-Site %q: %d %s", host, site, status, location)
			}
		}
	}
	if status, _ := get("attacker.example:"+port, ""); status != http.StatusForbidden {
		t.Errorf("other host name: %d; want 403", status)
	}
	for _, site := range []string{"cross-site", "same-site", "same-origin"} {
		if status, _ := get("127.0.0.1:"+port, site); status != http.StatusForbidden {
			t.Errorf("Sec-Fetch-Site %s: %d; want 403", site, status)
		}
	}
}

func TestOwnerList(t *testing.T) {
	var owners ownerList
	_ = owners.Set("Other, acme")
	_ = owners.Set("ACME")
	if got := strings.Join(owners.sorted(), ","); got != "acme,other" {
		t.Error(got)
	}
}

func TestRunUsage(t *testing.T) {
	for _, args := range [][]string{{}, {"--port", "1"}, {"--owner", "acme", "extra"}, {"--nope"}} {
		if err := run(args, io.Discard); err != errUsage {
			t.Errorf("run(%q) = %v; want a usage error", args, err)
		}
	}
	var out strings.Builder
	if err := run([]string{"--version"}, &out); err != nil || !strings.Contains(out.String(), version) {
		t.Errorf("--version: %v, %q", err, out.String())
	}
}

func TestSearchURLIsEscaped(t *testing.T) {
	a, _ := newTestApp(t, nil)
	got := a.destination("a&b=c")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if q := u.Query().Get("q"); q != "is:pr user:acme user:other a&b=c sort:created-desc" {
		t.Error(q)
	}
}
