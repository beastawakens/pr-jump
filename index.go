package main

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	_ "modernc.org/sqlite"
)

const (
	rebuildEvery     = 24 * time.Hour
	retryFailedAfter = 10 * time.Minute
	missRefreshEvery = 15 * time.Second
	searchPageSize   = 100
	searchMaxPages   = 10 // Search stops at 1,000 results.
	rebuildWorkers   = 8
)

const reposQuery = `query($owner: String!, $after: String) {
  repositoryOwner(login: $owner) { repositories(first: 100, after: $after, ownerAffiliations: OWNER) {
    nodes { nameWithOwner } pageInfo { hasNextPage endCursor } } } }`

const prsQuery = `query($owner: String!, $name: String!, $after: String) {
  repository(owner: $owner, name: $name) { pullRequests(first: 100, after: $after) {
    nodes { number createdAt } pageInfo { hasNextPage endCursor } } } }`

const schema = `
create table if not exists prs (repo text, number integer, created_at text, primary key (repo, number));
create index if not exists prs_number on prs (number);
create table if not exists meta (key text primary key, value real);`

// row is one indexed pull request.
type row struct {
	repo      string
	number    int
	createdAt string
}

func openIndex(path string) (*sql.DB, error) {
	// shortcut: a path containing "?" would break this DSN; quote it if anyone needs one.
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// save adds rows. With replace, it first deletes every row except those of the repos (or
// "owner/" prefixes) in keep, and records when this rebuild happened.
func (a *app) save(rows []row, replace bool, keep []string) error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if replace {
		query, args := "delete from prs", []any{}
		if len(keep) > 0 {
			var clauses []string
			for _, k := range keep {
				if strings.HasSuffix(k, "/") {
					clauses, args = append(clauses, "repo like ?"), append(args, k+"%")
				} else {
					clauses, args = append(clauses, "repo = ?"), append(args, k)
				}
			}
			query += " where not (" + strings.Join(clauses, " or ") + ")"
		}
		if _, err := tx.Exec(query, args...); err != nil {
			return err
		}
		rebuiltAt := a.now()
		if len(keep) > 0 { // Something failed, so make the next rebuild due soon.
			rebuiltAt = rebuiltAt.Add(retryFailedAfter - rebuildEvery)
		}
		if _, err := tx.Exec("insert or replace into meta values ('rebuilt_at', ?)", epoch(rebuiltAt)); err != nil {
			return err
		}
	}
	insert, err := tx.Prepare("insert or replace into prs values (?, ?, ?)")
	if err != nil {
		return err
	}
	defer insert.Close()
	for _, r := range rows {
		if _, err := insert.Exec(r.repo, r.number, r.createdAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func epoch(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

// rebuild fetches every PR of every repository the owners own and replaces the index. A
// repository that fails keeps its old rows.
func (a *app) rebuild() error {
	var repos, failed []string
	for _, owner := range a.owners {
		nodes, err := pages[struct{ NameWithOwner string }](a, reposQuery, map[string]any{"owner": owner},
			"repositoryOwner", "repositories")
		if err != nil {
			a.log.Printf("could not list repositories for %s: %v", owner, err)
			failed = append(failed, owner+"/")
			continue
		}
		for _, n := range nodes {
			repos = append(repos, n.NameWithOwner)
		}
	}
	if len(repos) == 0 {
		return fmt.Errorf("no repositories found for %s", strings.Join(a.owners, ", "))
	}

	var (
		mu   sync.Mutex
		rows []row
		wg   sync.WaitGroup
		slot = make(chan struct{}, rebuildWorkers)
	)
	for _, repo := range repos {
		wg.Add(1)
		slot <- struct{}{}
		go func() {
			defer func() { <-slot; wg.Done() }()
			owner, name, _ := strings.Cut(repo, "/")
			prs, err := pages[struct {
				Number    int
				CreatedAt string
			}](a, prsQuery, map[string]any{"owner": owner, "name": name}, "repository", "pullRequests")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				a.log.Printf("could not index %s: %v", repo, err)
				failed = append(failed, repo)
				return
			}
			for _, p := range prs {
				rows = append(rows, row{repo, p.Number, p.CreatedAt})
			}
		}()
	}
	wg.Wait()
	if err := a.save(rows, true, failed); err != nil {
		return err
	}
	message := fmt.Sprintf("indexed %d PRs in %d repos", len(rows), len(repos))
	if len(failed) > 0 {
		message += fmt.Sprintf(", %d failed", len(failed))
	}
	a.log.Print(message)
	return nil
}

// refresh adds PRs opened since the newest one indexed. If allowed, it rebuilds instead when
// the index is empty or a day old, or when there are too many new PRs for search to list.
func (a *app) refresh(allowRebuild bool) error {
	var newest sql.NullString
	if err := a.db.QueryRow("select max(created_at) from prs").Scan(&newest); err != nil {
		return err
	}
	var rebuiltAt float64
	err := a.db.QueryRow("select value from meta where key = 'rebuilt_at'").Scan(&rebuiltAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if allowRebuild && (!newest.Valid || epoch(a.now())-rebuiltAt > rebuildEvery.Seconds()) {
		return a.rebuild()
	}
	if !newest.Valid {
		return nil
	}
	since, err := time.Parse(time.RFC3339, newest.String)
	if err != nil {
		return err
	}
	// Overlap by an hour because search indexes new PRs with a delay.
	query := strings.Join(a.userQualifiers(), " ") + " is:pr created:>=" +
		since.Add(-time.Hour).UTC().Format("2006-01-02T15:04:05Z")
	var rows []row
	for page := 1; page <= searchMaxPages; page++ {
		var result struct {
			IncompleteResults bool `json:"incomplete_results"`
			Items             []struct {
				RepositoryURL string `json:"repository_url"`
				Number        int    `json:"number"`
				CreatedAt     string `json:"created_at"`
			} `json:"items"`
		}
		params := url.Values{"q": {query}, "sort": {"created"}, "order": {"asc"},
			"per_page": {strconv.Itoa(searchPageSize)}, "page": {strconv.Itoa(page)}}
		if err := a.call(http.MethodGet, "/search/issues?"+params.Encode(), nil, &result); err != nil {
			return err
		}
		if result.IncompleteResults {
			a.log.Print("search returned incomplete results; trying again next time")
			return nil
		}
		for _, item := range result.Items {
			parts := strings.Split(item.RepositoryURL, "/")
			if len(parts) < 2 {
				continue
			}
			rows = append(rows, row{strings.Join(parts[len(parts)-2:], "/"), item.Number, item.CreatedAt})
		}
		if len(result.Items) < searchPageSize {
			return a.save(rows, false, nil)
		}
	}
	if allowRebuild {
		return a.rebuild()
	}
	return nil
}

func (a *app) userQualifiers() []string {
	var qualifiers []string
	for _, owner := range a.owners {
		qualifiers = append(qualifiers, "user:"+owner)
	}
	return qualifiers
}

// Up to 9 digits, so pasted IDs fall through to text search.
var numberPattern = regexp.MustCompile(`(?:^|[\s#/])(\d{1,9})(?:\s|$)`)

// parse splits a query into a PR number and a lower-case repository hint. ok is false when
// the query has no PR number.
func parse(query string) (number int, hint string, ok bool) {
	m := numberPattern.FindStringSubmatchIndex(query)
	if m == nil {
		return 0, strings.TrimSpace(query), false
	}
	number, _ = strconv.Atoi(query[m[2]:m[3]])
	hint = strings.TrimFunc(query[:m[0]]+" "+query[m[1]:], func(r rune) bool {
		return unicode.IsSpace(r) || r == '#' || r == '/'
	})
	return number, strings.ToLower(hint), true
}

// lookup returns the newest repository with this PR number, preferring an exact owner/repo
// match, then an exact repository name, then a partial match. It returns "" when none match.
func (a *app) lookup(number int, hint string) (string, error) {
	result, err := a.db.Query("select repo from prs where number = ? order by created_at desc", number)
	if err != nil {
		return "", err
	}
	defer result.Close()
	var repos []string
	for result.Next() {
		var repo string
		if err := result.Scan(&repo); err != nil {
			return "", err
		}
		repos = append(repos, repo)
	}
	if err := result.Err(); err != nil {
		return "", err
	}
	if hint == "" {
		if len(repos) == 0 {
			return "", nil
		}
		return repos[0], nil
	}
	matchers := []func(string) bool{
		func(r string) bool { return strings.ToLower(r) == hint },
		func(r string) bool { _, name, _ := strings.Cut(r, "/"); return strings.ToLower(name) == hint },
		func(r string) bool { return strings.Contains(strings.ToLower(r), hint) },
	}
	for _, matches := range matchers {
		for _, repo := range repos {
			if matches(repo) {
				return repo, nil
			}
		}
	}
	return "", nil
}
