package wsl

import "testing"

func TestWindowsNativePathMapping(t *testing.T) {
	for _, test := range []struct{ native, want string }{{`C:\Users\Person Name\Rex\bot`, "/mnt/c/Users/Person Name/Rex/bot"}, {`D:\Rex\a&b%#`, "/mnt/d/Rex/a&b%#"}} {
		got, err := NativePath(test.native)
		if err != nil || got != test.want {
			t.Fatalf("mapping %q: %q %v", test.native, got, err)
		}
	}
	if _, err := NativePath(`\\server\share\bot`); err == nil {
		t.Fatal("accepted an unverified network workspace")
	}
}

func TestWindowsWSLConfigRejectsUnboundedTargets(t *testing.T) {
	for _, config := range []Config{{Distribution: "--system", Helper: "/opt/sandbox", ProbeRoot: `C:\Rex`}, {Distribution: "Rex", Helper: "relative", ProbeRoot: `C:\Rex`}, {Distribution: "Rex", Helper: "/opt/sandbox", ProbeRoot: `C:\`}} {
		if config.Validate() == nil {
			t.Fatal("accepted invalid WSL config")
		}
	}
}
