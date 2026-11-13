package apps

import (
	"testing"

	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/pkg/identity"
	"github.com/tsic404/kosd/pkg/write"
)

// staticApps is a declarations source holding one installed application.
type staticApps []extension.InstalledApp

// Apps returns the applications the case installed.
func (s staticApps) Apps() []extension.InstalledApp { return s }

// installed returns one application declaration.
func installed() extension.InstalledApp {
	return extension.InstalledApp{
		Extension: "kos.files",
		Component: "kos-filemanager",
		Phase:     extension.PhaseReady,
		Digest:    "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		Manifest: extension.App{
			ID: "files", Name: "Files", Version: "1.0.0",
			Icon:  "assets/icon.svg",
			Entry: "https://files.apps.test/",
		},
	}
}

// TestListRequiresASession checks that an unsigned caller reads nothing (C-59,
// C-63).
func TestListRequiresASession(t *testing.T) {
	service := New(staticApps{installed()})
	if list := service.List(identity.Session{}); len(list) != 0 {
		t.Fatalf("list without a session = %+v, want none", list)
	}
}

// TestEveryRoleReadsTheInstalledSet checks C-59: the application projection is
// a read every signed-in role makes, Guest included.
func TestEveryRoleReadsTheInstalledSet(t *testing.T) {
	service := New(staticApps{installed()})
	for _, role := range []write.Role{write.RoleOwner, write.RoleMember, write.RoleGuest} {
		session := identity.Session{Username: "someone", Role: role}
		list := service.List(session)
		if len(list) != 1 || list[0].Manifest.ID != "files" {
			t.Fatalf("list for %s = %+v, want the installed application", role, list)
		}
	}
}
