package memory

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/vibed-project/mindD/internal/episodic"
	"github.com/vibed-project/mindD/internal/episodic/episodictest"
)

type harness struct{}

func (harness) New(t *testing.T) episodic.Driver {
	t.Helper()
	d := New(Options{})
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func (harness) TailSettleTime() time.Duration { return 50 * time.Millisecond }

func TestConformance(t *testing.T) {
	episodictest.RunConformance(t, harness{})
}

// Driver-specific: the in-memory driver detaches subscribers that fall behind
// its bounded per-subscriber buffer. Buffer size is an internal implementation
// detail, so this test lives in-package.
func TestTail_SlowSubscriberDetached(t *testing.T) {
	d := New(Options{})
	t.Cleanup(func() { _ = d.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- d.Tail(ctx, "ns", episodic.TailOptions{}, func(e episodic.Event) error {
			<-release
			return nil
		})
	}()
	time.Sleep(50 * time.Millisecond)

	for i := 0; i < subscriberBufferSize+5; i++ {
		_, _ = d.Append(ctx, "ns", episodic.AppendOptions{Type: "spam"})
	}

	close(release)

	select {
	case err := <-done:
		assert.ErrorIs(t, err, ErrSubscriberLagged)
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber was not detached")
	}
}
