// Command pr-jump lets you type a pull request number in your browser's address bar and go
// straight to that PR. It keeps a local SQLite index of every PR owned by the GitHub users
// or organisations you choose, and redirects http://127.0.0.1:7777/?q=<query> to the newest
// PR with that number.
package main

import (
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var version = "dev" // Set at release time.

// githubAPI is the GitHub API address. Tests point it at a fake.
var githubAPI = "https://api.github.com"

type app struct {
	owners   []string
	db       *sql.DB
	api      string // GitHub API base URL
	client   *http.Client
	token    func() (string, error)
	sleep    func(time.Duration)
	now      func() time.Time
	log      *log.Logger
	mu       sync.Mutex   // One refresh or rebuild at a time.
	lastMiss atomic.Int64 // Unix nanoseconds of the last refresh a lookup started.
}

func newApp(owners []string, logger *log.Logger) *app {
	return &app{
		owners: owners,
		api:    githubAPI,
		client: &http.Client{Timeout: 30 * time.Second},
		token:  ghToken,
		sleep:  time.Sleep,
		now:    time.Now,
		log:    logger,
	}
}

// destination returns the URL to send a query to.
func (a *app) destination(query string) string {
	if number, hint, ok := parse(query); ok {
		repo, err := a.lookup(number, hint)
		if err != nil {
			a.log.Printf("lookup failed: %v", err)
		}
		// Maybe it opened since the last refresh. Never wait for the background thread, and
		// never rebuild here: either would leave the browser hanging.
		if repo == "" && err == nil && a.now().UnixNano()-a.lastMiss.Load() > int64(missRefreshEvery) && a.mu.TryLock() {
			a.lastMiss.Store(a.now().UnixNano())
			if err := a.refresh(false); err != nil {
				a.log.Printf("refresh failed: %v", err)
			}
			a.mu.Unlock()
			repo, _ = a.lookup(number, hint)
		}
		if repo != "" {
			return fmt.Sprintf("https://github.com/%s/pull/%d", repo, number)
		}
	}
	search := "is:pr " + strings.Join(a.userQualifiers(), " ") + " " + strings.TrimSpace(query) + " sort:created-desc"
	return "https://github.com/pulls?q=" + url.QueryEscape(search)
}

// ServeHTTP answers only navigations the user started and pr-jump's own setup page, sent to
// localhost. Other sites' pages could otherwise probe the index (cross-site requests) or read
// it (DNS rebinding).
func (a *app) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	site := r.Header.Get("Sec-Fetch-Site")
	if (host != "127.0.0.1" && host != "localhost") || (site != "" && site != "none" && site != "same-origin") {
		http.Error(w, "pr-jump only answers addresses typed into this computer's browser", http.StatusForbidden)
		return
	}
	switch {
	case r.URL.Path == "/opensearch.xml":
		a.serveOpenSearch(w, r)
	case r.URL.Path == "/" && !r.URL.Query().Has("q"):
		a.serveSetup(w, r)
	default:
		http.Redirect(w, r, a.destination(r.URL.Query().Get("q")), http.StatusFound)
	}
}

func (a *app) background(every time.Duration) {
	for {
		a.mu.Lock()
		if err := a.refresh(true); err != nil {
			a.log.Printf("refresh failed: %v", err)
		}
		a.mu.Unlock()
		time.Sleep(every)
	}
}

// ownerList collects --owner values, which may repeat or be comma-separated.
type ownerList []string

func (o *ownerList) String() string { return strings.Join(*o, ",") }

func (o *ownerList) Set(value string) error {
	for _, owner := range strings.Split(value, ",") {
		if owner = strings.ToLower(strings.TrimSpace(owner)); owner != "" {
			*o = append(*o, owner)
		}
	}
	return nil
}

// sorted returns the owners sorted, without duplicates.
func (o ownerList) sorted() []string {
	seen := map[string]bool{}
	var out []string
	for _, owner := range o {
		if !seen[owner] {
			seen[owner] = true
			out = append(out, owner)
		}
	}
	sort.Strings(out)
	return out
}

func defaultDataDir() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "pr-jump")
}

const usage = `pr-jump: type a PR number in your browser's address bar and go straight to the PR.

Usage:
  pr-jump install --owner <user-or-org> [--owner ...] [--port 7777]   run at login
  pr-jump uninstall                                                    stop running at login
  pr-jump --owner <user-or-org> [--owner ...] [--port 7777]           run in this terminal
  pr-jump --owner <user-or-org> --rebuild                              refetch every PR, then exit
  pr-jump --owner <user-or-org> --validate                             check the owners exist

Then add a browser search shortcut for http://127.0.0.1:7777/?q=%s
See https://github.com/beastawakens/pr-jump for browser steps.

Options:
`

// errUsage marks a mistake in how pr-jump was run.
var errUsage = errors.New("usage")

func run(args []string, stderr io.Writer) (err error) {
	command := ""
	if len(args) > 0 && (args[0] == "install" || args[0] == "uninstall") {
		command, args = args[0], args[1:]
	}
	flags := flag.NewFlagSet("pr-jump", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var owners ownerList
	flags.Var(&owners, "owner", "GitHub user or organisation to index; repeat or comma-separate for more")
	port := flags.Int("port", 7777, "port to listen on, on 127.0.0.1")
	every := flags.Duration("refresh", 2*time.Minute, "how often to check for new PRs")
	dataDir := flags.String("data-dir", defaultDataDir(), "folder for the index")
	logFile := flags.String("log", "", "append log lines to this file instead of standard error")
	background := flags.Bool("background", false, "detach from the console (used by the Windows login item)")
	rebuild := flags.Bool("rebuild", false, "refetch every PR, then exit")
	validate := flags.Bool("validate", false, "check the owners exist, then exit")
	showVersion := flags.Bool("version", false, "print the version, then exit")
	noBrowser := flags.Bool("no-browser", false, "with install, do not open the setup page")
	flags.Usage = func() {
		_, _ = io.WriteString(stderr, usage)
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errUsage
	}
	if *background {
		detachConsole() // Closes the console window Windows opens for a login item.
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "pr-jump: unexpected argument %q\n", flags.Arg(0))
		return errUsage
	}
	if *showVersion {
		fmt.Fprintln(stderr, "pr-jump", version)
		return nil
	}
	dir, err := filepath.Abs(*dataDir)
	if err != nil {
		return err
	}
	if command == "uninstall" {
		return uninstall(newService(), dir, stderr)
	}
	if *every <= 0 {
		fmt.Fprintln(stderr, "pr-jump: --refresh must be longer than zero")
		return errUsage
	}
	if len(owners) == 0 {
		fmt.Fprintln(stderr, "pr-jump: name at least one --owner")
		flags.Usage()
		return errUsage
	}

	logger := log.New(stderr, "", log.LstdFlags)
	if *logFile != "" {
		f, openErr := os.OpenFile(*logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if openErr != nil {
			return openErr
		}
		defer f.Close()
		logger.SetOutput(f)
		// The Windows login item has nowhere else to report why it stopped.
		defer func() {
			if err != nil {
				logger.Printf("stopped: %v", err)
			}
		}()
	}
	a := newApp(owners.sorted(), logger)
	// Installing or validating needs a definite answer; serving must start even offline.
	missing, err := a.missingOwners(command == "install" || *validate)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("no GitHub user or organisation called %s", strings.Join(missing, ", "))
	}
	if *validate {
		return nil
	}
	// The index lists private repository names, so keep it to this account.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	if command == "install" {
		serveArgs := []string{"--port", fmt.Sprint(*port), "--refresh", every.String(), "--data-dir", dir}
		for _, owner := range a.owners {
			serveArgs = append(serveArgs, "--owner", owner)
		}
		setupURL := fmt.Sprintf("http://127.0.0.1:%d/", *port)
		if *noBrowser {
			setupURL = ""
		}
		return install(newService(), serveArgs, dir, setupURL, stderr)
	}

	if a.db, err = openIndex(filepath.Join(dir, strings.Join(a.owners, "+")+".db")); err != nil {
		return err
	}
	defer a.db.Close()
	if *rebuild {
		return a.rebuild()
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		return err
	}
	if *background {
		// Lets install and uninstall stop this copy, and only this copy.
		pidFile := filepath.Join(dir, pidFileName)
		if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			return err
		}
		defer os.Remove(pidFile)
	}
	go a.background(*every)
	a.log.Printf("serving http://127.0.0.1:%d/?q=%%s for %s", *port, strings.Join(a.owners, ", "))
	server := &http.Server{Handler: a, ReadHeaderTimeout: 10 * time.Second}
	return server.Serve(listener)
}

func main() {
	err := run(os.Args[1:], os.Stderr)
	switch {
	case errors.Is(err, errUsage):
		os.Exit(2)
	case err != nil:
		fmt.Fprintln(os.Stderr, "pr-jump:", err)
		os.Exit(1)
	}
}
