package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const apiAttempts = 4

// httpError is a non-2xx answer from GitHub.
type httpError struct {
	code int
	body string
}

func (e *httpError) Error() string { return fmt.Sprintf("GitHub returned HTTP %d: %s", e.code, e.body) }

const tokenLifetime = 5 * time.Minute

var tokenCache struct {
	sync.Mutex
	token   string
	fetched time.Time
}

// ghToken reads GITHUB_TOKEN, or asks the GitHub CLI. It keeps gh's answer for a few minutes,
// because a rebuild makes hundreds of requests.
func ghToken() (string, error) {
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		return token, nil
	}
	tokenCache.Lock()
	defer tokenCache.Unlock()
	if tokenCache.token != "" && time.Since(tokenCache.fetched) < tokenLifetime {
		return tokenCache.token, nil
	}
	gh, err := exec.LookPath("gh")
	if err != nil {
		return "", errors.New("set GITHUB_TOKEN, or install the GitHub CLI (gh) and run 'gh auth login'")
	}
	cmd := exec.Command(gh, "auth", "token")
	hideWindow(cmd) // On Windows, a login item has no console, so gh would open a window.
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh auth token: %w", err)
	}
	tokenCache.token, tokenCache.fetched = strings.TrimSpace(string(out)), time.Now()
	return tokenCache.token, nil
}

// call sends one GitHub API request and decodes the JSON answer into out, retrying network
// errors, server errors and rate limits.
func (a *app) call(method, path string, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	for attempt := 0; ; attempt++ {
		delay, err := a.try(method, path, payload, out)
		if err == nil {
			return nil
		}
		if delay < 0 || attempt == apiAttempts-1 {
			return err
		}
		if delay == 0 {
			delay = time.Second << attempt
		}
		a.sleep(delay)
	}
}

// try makes one request. A delay of 0 or more means the error is worth retrying, with 0
// meaning "use the usual backoff".
func (a *app) try(method, path string, payload []byte, out any) (time.Duration, error) {
	token, err := a.token()
	if err != nil {
		return -1, err
	}
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, a.api+path, body)
	if err != nil {
		return -1, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode >= 300 {
		e := &httpError{resp.StatusCode, strings.TrimSpace(string(data[:min(len(data), 200)]))}
		wait := resp.Header.Get("Retry-After")
		// 499 is GitHub's answer when a GraphQL query times out.
		if resp.StatusCode < 499 && resp.StatusCode != http.StatusTooManyRequests && wait == "" {
			return -1, e
		}
		seconds, _ := strconv.Atoi(wait)
		return time.Duration(seconds) * time.Second, e
	}
	if err := json.Unmarshal(data, out); err != nil {
		return -1, fmt.Errorf("decoding GitHub's answer: %w", err)
	}
	return 0, nil
}

// pages runs a paginated GraphQL query and returns every node of the connection at path.
func pages[T any](a *app, query string, vars map[string]any, path ...string) ([]T, error) {
	var all []T
	var after any // JSON null on the first page
	for {
		variables := map[string]any{"after": after}
		for k, v := range vars {
			variables[k] = v
		}
		var result struct {
			Data   json.RawMessage `json:"data"`
			Errors []any           `json:"errors"`
		}
		if err := a.call(http.MethodPost, "/graphql", map[string]any{"query": query, "variables": variables}, &result); err != nil {
			return nil, err
		}
		if len(result.Errors) > 0 {
			return nil, fmt.Errorf("GraphQL errors: %v", result.Errors)
		}
		raw := result.Data
		for _, key := range path {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
				return nil, fmt.Errorf("%s not found: %v", key, vars)
			}
			raw = fields[key]
			if len(raw) == 0 || string(raw) == "null" {
				return nil, fmt.Errorf("%s not found: %v", key, vars)
			}
		}
		var connection struct {
			Nodes    []T `json:"nodes"`
			PageInfo struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
		}
		if err := json.Unmarshal(raw, &connection); err != nil {
			return nil, err
		}
		all = append(all, connection.Nodes...)
		if !connection.PageInfo.HasNextPage {
			return all, nil
		}
		after = connection.PageInfo.EndCursor
	}
}

// missingOwners returns the owners GitHub says do not exist. When strict, any other error is
// returned too; otherwise it is logged and ignored, so the service still starts offline.
func (a *app) missingOwners(strict bool) ([]string, error) {
	var missing []string
	for _, owner := range a.owners {
		var user struct{}
		err := a.call(http.MethodGet, "/users/"+url.PathEscape(owner), nil, &user)
		var he *httpError
		switch {
		case errors.As(err, &he) && he.code == http.StatusNotFound:
			missing = append(missing, owner)
		case err != nil && strict:
			return nil, fmt.Errorf("could not check owner %s: %w", owner, err)
		case err != nil:
			a.log.Printf("could not check owner %s: %v", owner, err)
		}
	}
	return missing, nil
}
