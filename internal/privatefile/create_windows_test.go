package privatefile

import (
	"golang.org/x/sys/windows"
	"path/filepath"
	"testing"
	"unsafe"
)

func TestOwnerOnlyWindowsACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.db")
	file, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil || acl.AceCount != 1 {
		t.Fatalf("expected exactly one private access grant: %v", err)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(acl, 0, &ace); err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !sid.Equals(user.User.Sid) {
		t.Fatal("private file grants access to another principal")
	}
}
