package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func exitedBrowserOwner(t *testing.T) int {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^$")
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	return command.Process.Pid
}

func TestNativeExtensionProfileLockOwnership(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Chromium uses hostname/PID singleton symlinks on Unix")
	}
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	dead := exitedBrowserOwner(t)
	for _, test := range []struct {
		name, target string
		busy         bool
	}{
		{"stale", fmt.Sprintf("%s-%d", hostname, dead), false},
		{"live", fmt.Sprintf("%s-%d", hostname, os.Getpid()), true},
		{"foreign host", fmt.Sprintf("another-%s-%d", hostname, dead), true},
		{"unknown", "existing-browser", true},
		{"invalid pid", hostname + "-0", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			profile := filepath.Join(root, "saved-profile")
			if err := os.MkdirAll(profile, 0700); err != nil {
				t.Fatal(err)
			}
			lock := filepath.Join(profile, "SingletonLock")
			if err := os.Symlink(test.target, lock); err != nil {
				t.Fatal(err)
			}
			cookie := filepath.Join(profile, "SingletonCookie")
			if err := os.WriteFile(cookie, []byte("keep-cookie"), 0600); err != nil {
				t.Fatal(err)
			}
			m := newBrowserManager(BrowserToolsConfig{ProfileRoot: root, ProfileAppID: "app"})
			t.Cleanup(func() { _ = registryForBrowser(m).CloseBrowsers(context.Background()) })
			_, err := m.nativeExtension("app", "agent", "saved-profile")
			if test.busy && (err == nil || !strings.Contains(err.Error(), "profile is busy")) || !test.busy && err != nil {
				t.Fatalf("busy=%v: %v", test.busy, err)
			}
			if target, err := os.Readlink(lock); err != nil || target != test.target {
				t.Fatalf("runtime changed Chromium's lock: target=%q error=%v", target, err)
			}
			if data, err := os.ReadFile(cookie); err != nil || string(data) != "keep-cookie" {
				t.Fatalf("runtime changed profile data: %q %v", data, err)
			}
		})
	}
}

func testExtensionStaleProfile(t *testing.T, executable string) {
	if runtime.GOOS == "windows" {
		t.Skip("Chromium uses hostname/PID singleton symlinks on Unix")
	}
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	profile := filepath.Join(root, "saved-profile")
	if err := os.MkdirAll(profile, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fmt.Sprintf("%s-%d", hostname, exitedBrowserOwner(t)), filepath.Join(profile, "SingletonLock")); err != nil {
		t.Fatal(err)
	}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "<title>Recovered profile</title><p>Recovered stale browser profile</p>")
	}))
	defer fixture.Close()
	m := newBrowserManager(BrowserToolsConfig{ProfileRoot: root, ProfileAppID: "test", ExtensionExecutable: executable})
	t.Cleanup(func() { _ = registryForBrowser(m).CloseBrowsers(context.Background()) })
	session := &browserRunSession{appID: "test", agentID: "stale", runID: "stale-run", sessionName: "stale-run", policy: BrowserPolicy{Backend: "extension", Profile: "saved-profile"}}
	for _, command := range [][]string{{"open", fixture.URL}, {"get", "text", "body"}} {
		raw, err := m.nativeExtensionCommand(t.Context(), session, command)
		if err != nil {
			t.Fatal(err)
		}
		var result struct{ Success bool }
		if err := json.Unmarshal(raw, &result); err != nil || !result.Success {
			t.Fatalf("browser did not recover: %s %v", raw, err)
		}
		if command[0] == "get" && !strings.Contains(string(raw), "Recovered stale browser profile") {
			t.Fatalf("page did not load after recovering the lock: %s", raw)
		}
	}
}
