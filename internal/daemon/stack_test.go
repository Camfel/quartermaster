package daemon

import (
	"path/filepath"
	"testing"

	"quartermaster/pkg/config"
	"quartermaster/pkg/types"
)

func TestLoadMergedStackRejectsShortNameCollision(t *testing.T) {
	dir := t.TempDir()
	stackFile := filepath.Join(dir, "stack.yaml")
	cm := config.NewConfigManager()

	stack := &types.Stack{
		Version:  "1",
		Kind:     "Stack",
		Metadata: types.Metadata{Name: "test"},
		Spec: types.StackSpec{Services: []types.Service{
			{Name: "jellyfin-web", Image: "alpine", Network: "internal"},
			{Name: "jellyfin-api", Image: "alpine", Network: "internal"},
		}},
	}
	if err := cm.SaveStack(stackFile, stack); err != nil {
		t.Fatal(err)
	}

	d := &Daemon{
		configManager: cm,
		stackFile:     stackFile,
		settingsPath:  filepath.Join(dir, "settings.json"),
	}
	if _, err := d.loadMergedStack(); err == nil {
		t.Fatal("expected loadMergedStack to reject colliding network names")
	}
}
