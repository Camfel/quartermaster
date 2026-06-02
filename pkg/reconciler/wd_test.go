package reconciler

import (
	"os"
	"testing"
)

func TestWorkingDirectory(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Current working directory: %s", dir)
}
