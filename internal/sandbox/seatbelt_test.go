package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func argValue(args []string, key string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == "-D" && strings.HasPrefix(args[index+1], key+"=") {
			return strings.TrimPrefix(args[index+1], key+"=")
		}
	}
	return ""
}

func TestSeatbeltArgsPassPathsAsParameters(t *testing.T) {
	root := t.TempDir()
	tricky := filepath.Join(t.TempDir(), `weird") (allow file-write* (subpath "/`)
	args, err := SeatbeltArgs(SeatbeltPaths{Root: root, ReadWrite: []string{tricky, tricky}, DenyRead: []string{"relative/ignored"}}, "echo", []string{"hi"})
	if err != nil {
		t.Fatal(err)
	}
	if args[0] != "-p" {
		t.Fatalf("profile must come first: %v", args)
	}
	profile := args[1]
	if strings.Contains(profile, root) || strings.Contains(profile, "weird") {
		t.Fatalf("paths must never be interpolated into the profile:\n%s", profile)
	}
	if !strings.Contains(profile, `(subpath (param "READ_WRITE_0"))`) || strings.Contains(profile, "READ_WRITE_1") {
		t.Fatalf("duplicate read-write paths should collapse to one parameter:\n%s", profile)
	}
	if strings.Contains(profile, "DENY_READ_0") {
		t.Fatalf("relative deny paths must be dropped:\n%s", profile)
	}
	resolvedRoot, _ := filepath.EvalSymlinks(root)
	if argValue(args, "RUN_ROOT") != resolvedRoot {
		t.Fatalf("run root not canonicalized: %q want %q", argValue(args, "RUN_ROOT"), resolvedRoot)
	}
	tail := args[len(args)-3:]
	if strings.Join(tail, " ") != "-- echo hi" {
		t.Fatalf("program must follow --: %v", args)
	}
}

func TestSeatbeltArgsResolveSymlinkedRoots(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	args, err := SeatbeltArgs(SeatbeltPaths{Root: link, DenyRead: []string{filepath.Join(link, "not-yet-created")}}, "true", nil)
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := filepath.EvalSymlinks(real)
	if got := argValue(args, "RUN_ROOT"); got != resolved {
		t.Fatalf("Seatbelt matches resolved paths; got %q want %q", got, resolved)
	}
	if got := argValue(args, "DENY_READ_0"); got != filepath.Join(resolved, "not-yet-created") {
		t.Fatalf("missing paths resolve through their parent; got %q", got)
	}
}

func TestSeatbeltArgsRequireAbsoluteRoot(t *testing.T) {
	for _, root := range []string{"", "relative", "/"} {
		if _, err := SeatbeltArgs(SeatbeltPaths{Root: root}, "true", nil); err == nil {
			t.Fatalf("root %q should be rejected", root)
		}
	}
}
