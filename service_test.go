package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeService records the commands install and uninstall run, without running them.
func fakeService(t *testing.T, goos string) (service, *[]string) {
	t.Helper()
	home := t.TempDir()
	var ran []string
	record := func(name string, args ...string) error {
		ran = append(ran, strings.Join(append([]string{name}, args...), " "))
		return nil
	}
	return service{
		goos:      goos,
		home:      home,
		configDir: filepath.Join(home, ".config"),
		exe:       "/opt/pr jump/bin/pr-jump",
		gh:        "/opt/homebrew/bin/gh",
		pid:       4242,
		ppid:      4141,
		run:       record,
		start: func(name string, args ...string) error {
			return record("START "+name, args...)
		},
		sleep: func(time.Duration) {},
	}, &ran
}

var serveArgs = []string{"--port", "7777", "--refresh", "2m0s", "--data-dir", "/data/pr-jump", "--owner", "acme&co"}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// skipOnWindows skips tests of macOS and Linux paths, which use forward slashes.
func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("checks Unix paths")
	}
}

func TestInstallMacOS(t *testing.T) {
	skipOnWindows(t)
	s, ran := fakeService(t, "darwin")
	var out strings.Builder
	if err := install(s, serveArgs, "/data/pr-jump", &out); err != nil {
		t.Fatal(err)
	}
	plist := read(t, s.launchdPlistPath())
	for _, want := range []string{
		"<string>/opt/pr jump/bin/pr-jump</string>",
		"<string>acme&amp;co</string>", // escaped for XML
		"<string>/opt/homebrew/bin:/usr/bin:/bin</string>",
		"<string>/data/pr-jump/server.log</string>",
		"<key>KeepAlive</key><true/>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %s:\n%s", want, plist)
		}
	}
	if runtime.GOOS == "darwin" {
		if lint, err := exec.Command("plutil", "-lint", s.launchdPlistPath()).CombinedOutput(); err != nil {
			t.Errorf("plutil: %v: %s", err, lint)
		}
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	want := []string{"launchctl bootout " + domain + "/local.pr-jump", "launchctl bootstrap " + domain + " " + s.launchdPlistPath()}
	if strings.Join(*ran, "\n") != strings.Join(want, "\n") {
		t.Errorf("ran %q; want %q", *ran, want)
	}

	*ran = nil
	if err := uninstall(s, "/data/pr-jump", &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.launchdPlistPath()); !os.IsNotExist(err) {
		t.Error("uninstall left the launch agent")
	}
	if len(*ran) != 1 || !strings.HasPrefix((*ran)[0], "launchctl bootout") {
		t.Errorf("uninstall ran %q", *ran)
	}
}

func TestInstallLinux(t *testing.T) {
	skipOnWindows(t)
	s, ran := fakeService(t, "linux")
	var out strings.Builder
	if err := install(s, serveArgs, "/data/pr-jump", &out); err != nil {
		t.Fatal(err)
	}
	unit := read(t, s.systemdUnitPath())
	for _, want := range []string{
		`ExecStart="/opt/pr jump/bin/pr-jump" "--port" "7777"`,
		`"--owner" "acme&co"`,
		`Environment="PATH=/opt/homebrew/bin:/usr/bin:/bin"`,
		"Restart=always",
		"WantedBy=default.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %s:\n%s", want, unit)
		}
	}
	want := "systemctl --user daemon-reload\nsystemctl --user enable pr-jump.service\nsystemctl --user restart pr-jump.service"
	if got := strings.Join(*ran, "\n"); got != want {
		t.Errorf("ran:\n%s\nwant:\n%s", got, want)
	}

	*ran = nil
	if err := uninstall(s, "/data/pr-jump", &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.systemdUnitPath()); !os.IsNotExist(err) {
		t.Error("uninstall left the unit file")
	}
	if got := strings.Join(*ran, "\n"); got != "systemctl --user disable --now pr-jump.service\nsystemctl --user daemon-reload" {
		t.Errorf("uninstall ran %q", got)
	}
}

func TestInstallWindows(t *testing.T) {
	s, ran := fakeService(t, "windows")
	s.exe = `C:\Program Files\pr-jump\pr-jump.exe`
	dataDir := t.TempDir()
	logPath := filepath.Join(dataDir, "server.log")
	var out strings.Builder

	// No login item running yet: nothing to stop.
	if err := install(s, serveArgs, dataDir, &out); err != nil {
		t.Fatal(err)
	}
	commandLine := `"C:\Program Files\pr-jump\pr-jump.exe" --port 7777 --refresh 2m0s --data-dir /data/pr-jump --owner acme&co --background --log ` +
		windowsCommandLine([]string{logPath})
	want := []string{
		`reg add HKCU\Software\Microsoft\Windows\CurrentVersion\Run /v pr-jump /t REG_SZ /d ` + commandLine + ` /f`,
		`START C:\Program Files\pr-jump\pr-jump.exe --port 7777 --refresh 2m0s --data-dir /data/pr-jump --owner acme&co --background --log ` + logPath,
	}
	if strings.Join(*ran, "\n") != strings.Join(want, "\n") {
		t.Errorf("ran:\n%s\nwant:\n%s", strings.Join(*ran, "\n"), strings.Join(want, "\n"))
	}

	// The login item is running: stop that process only, and only if it is still pr-jump.
	if err := os.WriteFile(filepath.Join(dataDir, pidFileName), []byte("999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	*ran = nil
	if err := install(s, serveArgs, dataDir, &out); err != nil {
		t.Fatal(err)
	}
	if len(*ran) != 3 || (*ran)[1] != "taskkill /F /PID 999 /FI IMAGENAME eq pr-jump.exe" {
		t.Errorf("ran %q", *ran)
	}

	*ran = nil
	if err := uninstall(s, dataDir, &out); err != nil {
		t.Fatal(err)
	}
	if len(*ran) != 2 || !strings.HasPrefix((*ran)[0], "reg delete") || (*ran)[1] != "taskkill /F /PID 999 /FI IMAGENAME eq pr-jump.exe" {
		t.Errorf("uninstall ran %q", *ran)
	}
}

func TestStopWindowsSparesThisProcessAndItsParent(t *testing.T) {
	s, ran := fakeService(t, "windows")
	dataDir := t.TempDir()
	for _, pid := range []string{"4242", "4141", "not a number"} {
		if err := os.WriteFile(filepath.Join(dataDir, pidFileName), []byte(pid), 0o600); err != nil {
			t.Fatal(err)
		}
		s.stopWindows(dataDir)
	}
	if len(*ran) != 0 {
		t.Errorf("ran %q", *ran)
	}
}

func TestInstallNeedsGh(t *testing.T) {
	s, ran := fakeService(t, "darwin")
	s.gh = ""
	if err := install(s, serveArgs, "/data", &strings.Builder{}); err == nil || !strings.Contains(err.Error(), "gh auth login") {
		t.Errorf("err = %v", err)
	}
	if len(*ran) != 0 {
		t.Errorf("ran %q", *ran)
	}
}

func TestInstallUnsupportedSystem(t *testing.T) {
	s, _ := fakeService(t, "plan9")
	if err := install(s, serveArgs, "/data", &strings.Builder{}); err == nil {
		t.Error("want an error")
	}
}

func TestQuoting(t *testing.T) {
	if got := systemdQuote(`a "b" 100% $HOME \x`); got != `"a \"b\" 100%% $$HOME \\x"` {
		t.Error(got)
	}
	if got := windowsCommandLine([]string{`C:\a b\x.exe`, "plain", "", `say "hi"`}); got != `"C:\a b\x.exe" plain "" "say \"hi\""` {
		t.Error(got)
	}
}

func TestExecutablePathPrefersStableLinks(t *testing.T) {
	// A version folder with a "current" link to it, as Scoop lays out apps.
	root := t.TempDir()
	versioned := filepath.Join(root, "1.2.3")
	if err := os.MkdirAll(versioned, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(versioned, "pr-jump")
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(versioned, filepath.Join(root, "current")); err != nil {
		t.Skip("cannot make symlinks here:", err)
	}
	if !sameFile(filepath.Join(root, "current", "pr-jump"), exe) || sameFile(filepath.Join(root, "nope"), exe) {
		t.Error("sameFile did not follow the link")
	}
}

func TestInstallMacOSRetriesBootstrapWhileOldJobStops(t *testing.T) {
	s, _ := fakeService(t, "darwin")
	bootstraps := 0
	s.run = func(name string, args ...string) error {
		if len(args) > 0 && args[0] == "bootstrap" {
			if bootstraps++; bootstraps < 3 {
				return fmt.Errorf("Bootstrap failed: 5: Input/output error")
			}
		}
		return nil
	}
	if err := install(s, serveArgs, "/data", &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if bootstraps != 3 {
		t.Errorf("%d bootstrap attempts; want 3", bootstraps)
	}
}
