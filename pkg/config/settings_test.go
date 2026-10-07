package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSettingsRedacted(t *testing.T) {
	s := &Settings{
		ComponentToken: "component-secret",
		Alerting:       AlertingSettings{GotifyToken: "gotify-secret"},
		Repos: []RepoConfig{
			{URL: "https://example/repo.git", Token: "git-secret"},
			{URL: "https://example/public.git"},
		},
	}
	r := s.Redacted()

	if r.ComponentToken != "***" || r.Alerting.GotifyToken != "***" || r.Repos[0].Token != "***" {
		t.Fatalf("tokens were not redacted: %+v", r)
	}
	if r.Repos[1].Token != "" {
		t.Errorf("empty token should stay empty, got %q", r.Repos[1].Token)
	}
	if r.Repos[0].URL != "https://example/repo.git" {
		t.Errorf("redaction dropped non-secret fields: %+v", r.Repos[0])
	}

	// The original must not be mutated.
	if s.ComponentToken != "component-secret" || s.Repos[0].Token != "git-secret" {
		t.Error("Redacted mutated the original settings")
	}
}

func TestSaveSettingsPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := SaveSettings(path, &Settings{}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("expected settings file mode 0600, got %o", perm)
	}

	// A previously looser file must be tightened on rewrite.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SaveSettings(path, &Settings{}); err != nil {
		t.Fatal(err)
	}
	fi, _ = os.Stat(path)
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("expected mode to be re-tightened to 0600, got %o", perm)
	}
}
