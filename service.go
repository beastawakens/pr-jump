package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	launchdLabel  = "local.pr-jump"
	systemdUnit   = "pr-jump.service"
	windowsRunKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
	pidFileName   = "pr-jump.pid" // Written by the Windows login item.
)

// service holds what install and uninstall need from the system, so tests can replace it.
type service struct {
	goos      string
	home      string
	configDir string // XDG config folder, for systemd
	exe       string // this program
	gh        string // the GitHub CLI, or "" if missing
	pid       int
	ppid      int                                     // Scoop's shim, also called pr-jump.exe, when run through Scoop
	run       func(name string, args ...string) error // runs a command and waits
	start     func(name string, args ...string) error // starts a command in the background
	sleep     func(time.Duration)
	ready     func(url string) bool // waits until the new login item answers
}

func newService() service {
	home, _ := os.UserHomeDir()
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		configDir = filepath.Join(home, ".config")
	}
	gh, _ := exec.LookPath("gh")
	if gh != "" {
		gh, _ = filepath.Abs(gh)
	}
	return service{
		goos:      runtime.GOOS,
		home:      home,
		configDir: configDir,
		exe:       executablePath(),
		gh:        gh,
		pid:       os.Getpid(),
		ppid:      os.Getppid(),
		run: func(name string, args ...string) error {
			out, err := exec.Command(name, args...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
			}
			return nil
		},
		start: startDetached,
		sleep: time.Sleep,
		ready: waitUntilServing,
	}
}

// waitUntilServing polls the login item for up to 10 seconds.
func waitUntilServing(url string) bool {
	client := &http.Client{Timeout: time.Second}
	for i := 0; i < 20; i++ {
		if resp, err := client.Get(url); err == nil {
			resp.Body.Close()
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// openBrowser opens a page in the default browser.
func (s service) openBrowser(url string) error {
	switch s.goos {
	case "darwin":
		return s.start("open", url)
	case "windows":
		return s.start("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return s.start("xdg-open", url)
	}
}

// executablePath returns a path to this program that still works after an upgrade: Homebrew's
// bin symlink, or Scoop's "current" folder, rather than a folder named after this version.
func executablePath() string {
	exe, err := os.Executable()
	if err != nil {
		return "pr-jump"
	}
	candidates := []string{filepath.Join(filepath.Dir(filepath.Dir(exe)), "current", filepath.Base(exe))}
	if onPath, err := exec.LookPath("pr-jump"); err == nil {
		candidates = append([]string{onPath}, candidates...)
	}
	for _, candidate := range candidates {
		if sameFile(candidate, exe) {
			if abs, err := filepath.Abs(candidate); err == nil {
				return abs
			}
		}
	}
	return exe
}

func sameFile(a, b string) bool {
	infoA, errA := os.Stat(a)
	infoB, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(infoA, infoB)
}

func (s service) launchdPlistPath() string {
	return filepath.Join(s.home, "Library", "LaunchAgents", launchdLabel+".plist")
}

func (s service) systemdUnitPath() string {
	return filepath.Join(s.configDir, "systemd", "user", systemdUnit)
}

// servicePath is the PATH a login item gets: enough to find gh, which supplies the token.
func (s service) servicePath() string {
	if s.goos == "windows" {
		return ""
	}
	return filepath.Dir(s.gh) + ":/usr/bin:/bin"
}

// install sets pr-jump up as a login item and starts it. If setupURL is not empty, it then
// opens the setup page there, which helps add the browser shortcut. firstIndex says there is
// no index yet, so the first one will take a while.
func install(s service, serveArgs []string, dataDir, setupURL string, firstIndex bool, out io.Writer) error {
	if s.gh == "" {
		return errors.New("install the GitHub CLI (gh) and run 'gh auth login' first: " +
			"the login item gets its token from gh, because it cannot see GITHUB_TOKEN from your shell")
	}
	logPath := filepath.Join(dataDir, "server.log")
	logHint := "its log goes to " + logPath
	args := append([]string{s.exe}, serveArgs...)
	switch s.goos {
	case "darwin":
		path := s.launchdPlistPath()
		if err := writeFile(path, launchdPlist(args, s.servicePath(), logPath)); err != nil {
			return err
		}
		domain := fmt.Sprintf("gui/%d", os.Getuid())
		_ = s.run("launchctl", "bootout", domain+"/"+launchdLabel) // Not loaded yet is fine.
		// bootout returns before the old job has gone, and until it has, bootstrap fails with
		// "Bootstrap failed: 5: Input/output error". Retry for up to 10 seconds.
		var err error
		for attempt := 0; attempt < 20; attempt++ {
			if err = s.run("launchctl", "bootstrap", domain, path); err == nil {
				break
			}
			s.sleep(500 * time.Millisecond)
		}
		if err != nil {
			return err
		}
	case "linux":
		if err := writeFile(s.systemdUnitPath(), systemdUnitFile(args, s.servicePath())); err != nil {
			return err
		}
		for _, step := range [][]string{{"daemon-reload"}, {"enable", systemdUnit}, {"restart", systemdUnit}} {
			if err := s.run("systemctl", append([]string{"--user"}, step...)...); err != nil {
				return err
			}
		}
		logHint = "see its log with: journalctl --user -u pr-jump -f"
	case "windows":
		args = append(args, "--background", "--log", logPath)
		if err := s.run("reg", "add", windowsRunKey, "/v", "pr-jump", "/t", "REG_SZ", "/d", windowsCommandLine(args), "/f"); err != nil {
			return err
		}
		s.stopWindows(dataDir)
		if err := s.start(args[0], args[1:]...); err != nil {
			return err
		}
	default:
		return fmt.Errorf("pr-jump cannot install itself on %s; run it with --owner from your own startup script", s.goos)
	}
	if firstIndex {
		fmt.Fprintf(out, "Installed. pr-jump now starts at login. The first index takes a minute or two; %s.\n", logHint)
	} else {
		fmt.Fprintf(out, "Installed. pr-jump now starts at login; %s.\n", logHint)
	}
	if setupURL != "" {
		if s.ready(setupURL) && s.openBrowser(setupURL) == nil {
			fmt.Fprintf(out, "Opening %s in your browser. It shows how to add the pr shortcut; if nothing opens, go there yourself.\n", setupURL)
		} else {
			fmt.Fprintf(out, "Next, open %s in your browser. It shows how to add the pr shortcut.\n", setupURL)
		}
	}
	return nil
}

func uninstall(s service, dataDir string, out io.Writer) error {
	switch s.goos {
	case "darwin":
		_ = s.run("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), launchdLabel))
		if err := removeIfExists(s.launchdPlistPath()); err != nil {
			return err
		}
	case "linux":
		_ = s.run("systemctl", "--user", "disable", "--now", systemdUnit)
		if err := removeIfExists(s.systemdUnitPath()); err != nil {
			return err
		}
		_ = s.run("systemctl", "--user", "daemon-reload")
	case "windows":
		_ = s.run("reg", "delete", windowsRunKey, "/v", "pr-jump", "/f")
		s.stopWindows(dataDir)
	default:
		return fmt.Errorf("pr-jump does not install itself on %s, so there is nothing to remove", s.goos)
	}
	fmt.Fprintf(out, "Removed. pr-jump no longer starts at login. The index is still in %s.\n", dataDir)
	return nil
}

// stopWindows ends the running login item, so a new one can use the port. It stops only the
// process the login item recorded, and only if that process is still pr-jump, so copies
// started by hand keep running.
func (s service) stopWindows(dataDir string) {
	data, err := os.ReadFile(filepath.Join(dataDir, pidFileName))
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid == s.pid || pid == s.ppid {
		return
	}
	_ = s.run("taskkill", "/F", "/PID", strconv.Itoa(pid), "/FI", "IMAGENAME eq pr-jump.exe")
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func launchdPlist(args []string, pathEnv, logPath string) string {
	var program strings.Builder
	for _, arg := range args {
		program.WriteString("\n    <string>" + xmlText(arg) + "</string>")
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + launchdLabel + `</string>
  <key>ProgramArguments</key>
  <array>` + program.String() + `
  </array>
  <key>EnvironmentVariables</key>
  <dict><key>PATH</key><string>` + xmlText(pathEnv) + `</string></dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardErrorPath</key><string>` + xmlText(logPath) + `</string>
</dict>
</plist>
`
}

// systemdQuote quotes one word of a unit file, where % and $ are special.
func systemdQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$").Replace(s) + `"`
}

func systemdUnitFile(args []string, pathEnv string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = systemdQuote(arg)
	}
	return `[Unit]
Description=pr-jump: go from a PR number to the pull request

[Service]
ExecStart=` + strings.Join(quoted, " ") + `
Environment=` + systemdQuote("PATH="+pathEnv) + `
Restart=always

[Install]
WantedBy=default.target
`
}

// windowsCommandLine joins arguments for the Run key, quoting any with spaces or quotes.
func windowsCommandLine(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		if arg == "" || strings.ContainsAny(arg, " \t\"") {
			arg = `"` + strings.ReplaceAll(arg, `"`, `\"`) + `"`
		}
		quoted[i] = arg
	}
	return strings.Join(quoted, " ")
}
