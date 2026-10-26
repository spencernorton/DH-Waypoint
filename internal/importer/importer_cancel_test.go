package importer

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestImportCancellationUnblocksDispatchAndDrain(t *testing.T) {
	for _, accepted := range []int{0, 1} {
		t.Run(string(rune('0'+accepted)), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			i := &activeImport{ctx: ctx, mutex: &sync.RWMutex{}, wg: &sync.WaitGroup{}, itemChan: make(chan Item)}
			done := make(chan error, 1)
			go func() { done <- i.Import(Item{}, Item{}, Item{}) }()
			for range accepted {
				select {
				case <-i.itemChan:
					i.wg.Done() // This accepted item has finished its simulated buffer work.
				case <-time.After(time.Second):
					t.Fatal("dispatch did not start")
				}
			}
			cancel()
			select {
			case err := <-done:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(time.Second):
				t.Fatal("canceled dispatch remained blocked without a receiver")
			}
			drained := make(chan struct{})
			go func() { i.Drain(); close(drained) }()
			select {
			case <-drained:
			case <-time.After(time.Second):
				t.Fatal("undispatched items leaked into the drain wait group")
			}
		})
	}
}
