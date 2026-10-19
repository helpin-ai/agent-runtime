package wsl

import "testing"

func TestInteropPolicyRequiresExplicitSectionAndBothKeys(t *testing.T) {
	for _, test := range []struct {
		config string
		valid  bool
	}{
		{"[interop]\nenabled=false\nappendWindowsPath=false\n", true},
		{"[interop]\nenabled = FALSE\nappendWindowsPath = false # disabled\n", true},
		{"[other]\nenabled=false\nappendWindowsPath=false", false},
		{"[interop]\n# enabled=false\nappendWindowsPath=false", false},
		{"[interop]\nenabled=false\nappendWindowsPath=false\nenabled=true", false},
		{"[interop]\nenabled=false", false},
	} {
		if got := InteropDisabled([]byte(test.config)); got != test.valid {
			t.Errorf("policy validity=%v, expected %v", got, test.valid)
		}
	}
}
