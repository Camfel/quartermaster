package daemon

import (
	"sync"
	"testing"

	"quartermaster/pkg/cri"
	"quartermaster/pkg/types"
)

// TestStatusConcurrentAccess exercises the Status methods from several
// goroutines.  It is specifically meaningful under `go test -race`, where it
// would flag the unsynchronized reads/writes the status API used to make.
func TestStatusConcurrentAccess(t *testing.T) {
	s := &Status{Version: "test"}
	stack := &types.Stack{}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			recordReconcile(s, nil)
			recordContainers(s, []cri.ContainerInfo{{Name: "svc"}}, stack)
			s.setContainerHealth("svc", true)
			s.setLKG(i%2 == 0, "")
		}
	}()

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				snap := s.snapshot()
				_ = snap.Containers
				_ = s.lkgHealthy()
				_ = s.stack()
			}
		}()
	}
	wg.Wait()
}
