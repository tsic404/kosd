package extension

import (
	"testing"
	"time"
)

// appRequest packs one application declaration into an install request.
func appRequest(t *testing.T, component, identifier string) Request {
	t.Helper()
	app := validApp()
	app.Icon = "https://files.apps.test/icon.svg"
	return packageRequest(t, component, appDeclaration(identifier, app), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
}

// TestRegistryAppsCarryTheDeclaringComponent checks the projection KOSD-C-73
// needs: the extension, the component, the phase, the digest and the manifest.
func TestRegistryAppsCarryTheDeclaringComponent(t *testing.T) {
	request := appRequest(t, "kos-filemanager", "kos.filemanager")
	assessment := Evaluate([]Request{request})
	apps := assessment.Registry.Apps()
	if len(apps) != 1 {
		t.Fatalf("apps = %+v, want one", apps)
	}
	installed := apps[0]
	if installed.Extension != "kos.filemanager" || installed.Component != "kos-filemanager" {
		t.Fatalf("installed app = %+v, want its declaring component", installed)
	}
	if installed.Phase != PhaseReady {
		t.Fatalf("phase = %s, want %s", installed.Phase, PhaseReady)
	}
	artifact, err := request.Package.Verify()
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if installed.Digest == "" || installed.Digest != artifact.Digest {
		t.Fatalf("digest = %q, want the verified package digest %q", installed.Digest, artifact.Digest)
	}
	if installed.Manifest.ID != "files" || installed.Manifest.Entry != "https://files.apps.test/" {
		t.Fatalf("manifest = %+v, want the declared application", installed.Manifest)
	}
	if apps := assessment.Registry.AppsOf("kos.filemanager"); len(apps) != 1 {
		t.Fatalf("AppsOf(declared) = %+v, want one", apps)
	}
	if apps := assessment.Registry.AppsOf("other.extension"); len(apps) != 0 {
		t.Fatalf("AppsOf(other) = %+v, want none", apps)
	}
}

// TestRegistryAppsDisappearWhenTheComponentStops checks C-28: a disabled
// component withdraws its applications, and two extensions may reuse one
// application id because uniqueness is per extension (KOSD-C-73).
func TestRegistryAppsDisappearWhenTheComponentStops(t *testing.T) {
	first := appRequest(t, "first", "demo.first")
	second := appRequest(t, "second", "demo.second")
	assessment := Evaluate([]Request{first, second})
	if apps := assessment.Registry.Apps(); len(apps) != 2 {
		t.Fatalf("apps = %+v, want both extensions' applications", apps)
	}

	disabled := appRequest(t, "first", "demo.first")
	disabled.IsDisabled = true
	assessment = Evaluate([]Request{disabled, second})
	if apps := assessment.Registry.Apps(); len(apps) != 1 || apps[0].Extension != "demo.second" {
		t.Fatalf("apps = %+v, want only the running component's application", apps)
	}

	assessment = Evaluate(nil)
	if apps := assessment.Registry.Apps(); len(apps) != 0 {
		t.Fatalf("apps after every component is gone = %+v, want none", apps)
	}
}

// TestInvalidApplicationClassLeavesTheComponentNotReady checks KOSD-C-18: an
// invalid App declaration is not ready and contributes no application.
func TestInvalidApplicationClassLeavesTheComponentNotReady(t *testing.T) {
	app := validApp()
	app.Icon = "assets/icon.svg"
	request := packageRequest(t, "broken", appDeclaration("demo.broken", app), time.Now())
	assessment := Evaluate([]Request{request})
	result := assessment.Components[0]
	if result.Phase != PhaseDegraded {
		t.Fatalf("phase = %s, want %s", result.Phase, PhaseDegraded)
	}
	if apps := assessment.Registry.Apps(); len(apps) != 0 {
		t.Fatalf("apps = %+v, want none without a carried icon", apps)
	}
}
