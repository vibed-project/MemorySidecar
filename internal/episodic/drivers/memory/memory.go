// Package memory implements an in-memory episodic driver.
//
// Events are stored per-namespace in an append-only slice. Cursors are
// assigned monotonically starting at 1. Live tailers each hold a buffered
// channel; slow tailers whose buffer fills up are detached with an error.
package memory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"memsidecar/internal/episodic"
)

// subscriberBufferSize bounds how far a live tailer may lag before being
// dropped. 256 covers most local-dev workloads; production will want backed
// pressure or a different driver entirely.
const subscriberBufferSize = 256

// ErrSubscriberLagged is returned to a Tail subscriber that fell behind.
var ErrSubscriberLagged = errors.New("episodic/memory: subscriber lagged and was detached")

// Options configures a Driver.
type Options struct {
	NowFunc func() time.Time
	NewID   func() string
}

// Driver is an in-memory episodic driver. Safe for concurrent use.
type Driver struct {
	mu      sync.Mutex
	streams map[string]*stream // namespace -> stream
	closed  bool
	now     func() time.Time
	newID   func() string
}

type stream struct {
	events []episodic.Event
	// subscribers receive events appended AFTER they were registered.
	subscribers []*subscriber
}

type subscriber struct {
	ch     chan episodic.Event
	err    chan error // closed/written on detach
	closed bool       // protected by Driver.mu
}

// New builds a Driver.
func New(opts Options) *Driver {
	d := &Driver{
		streams: make(map[string]*stream),
		now:     time.Now,
		newID:   randomID,
	}
	if opts.NowFunc != nil {
		d.now = opts.NowFunc
	}
	if opts.NewID != nil {
		d.newID = opts.NewID
	}
	return d
}

func (d *Driver) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	for _, s := range d.streams {
		for _, sub := range s.subscribers {
			if !sub.closed {
				sub.closed = true
				close(sub.ch)
				close(sub.err)
			}
		}
		s.subscribers = nil
	}
	return nil
}

// Size returns the number of events retained in namespace.
func (d *Driver) Size(_ context.Context, namespace string) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s := d.streams[namespace]; s != nil {
		return int64(len(s.events)), nil
	}
	return 0, nil
}

func (d *Driver) Append(_ context.Context, namespace string, opts episodic.AppendOptions) (episodic.Event, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed {
		return episodic.Event{}, errors.New("episodic/memory: driver closed")
	}

	s := d.streams[namespace]
	if s == nil {
		s = &stream{}
		d.streams[namespace] = s
	}

	cursor := uint64(len(s.events)) + 1
	ev := episodic.Event{
		ID:        d.newID(),
		Cursor:    cursor,
		Timestamp: d.now().UTC(),
		Type:      opts.Type,
		Payload:   cloneBytes(opts.Payload),
		Metadata:  cloneMeta(opts.Metadata),
	}
	s.events = append(s.events, ev)

	// Fan out to subscribers. Slow ones are detached.
	live := s.subscribers[:0]
	for _, sub := range s.subscribers {
		if sub.closed {
			continue
		}
		select {
		case sub.ch <- ev:
			live = append(live, sub)
		default:
			sub.closed = true
			sub.err <- ErrSubscriberLagged
			close(sub.ch)
			close(sub.err)
		}
	}
	s.subscribers = live
	return ev, nil
}

func (d *Driver) Range(_ context.Context, namespace string, opts episodic.RangeOptions, yield func(episodic.Event) error) error {
	d.mu.Lock()
	s := d.streams[namespace]
	if s == nil {
		d.mu.Unlock()
		return nil
	}
	// Snapshot to avoid holding the lock across yield.
	out := make([]episodic.Event, 0, len(s.events))
	for _, ev := range s.events {
		if opts.AfterCursor > 0 && ev.Cursor <= opts.AfterCursor {
			continue
		}
		if opts.BeforeCursor > 0 && ev.Cursor >= opts.BeforeCursor {
			continue
		}
		out = append(out, ev)
	}
	d.mu.Unlock()

	if opts.Reverse {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	if opts.Limit > 0 && uint32(len(out)) > opts.Limit {
		out = out[:opts.Limit]
	}
	for _, ev := range out {
		if err := yield(ev); err != nil {
			return err
		}
	}
	return nil
}

func (d *Driver) Tail(ctx context.Context, namespace string, opts episodic.TailOptions, yield func(episodic.Event) error) error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return errors.New("episodic/memory: driver closed")
	}
	s := d.streams[namespace]
	if s == nil {
		s = &stream{}
		d.streams[namespace] = s
	}

	var historical []episodic.Event
	headCursor := uint64(len(s.events))
	if opts.IncludeHistorical {
		historical = make([]episodic.Event, 0, len(s.events))
		for _, ev := range s.events {
			if ev.Cursor > opts.AfterCursor {
				historical = append(historical, ev)
			}
		}
	}

	sub := &subscriber{
		ch:  make(chan episodic.Event, subscriberBufferSize),
		err: make(chan error, 1),
	}
	s.subscribers = append(s.subscribers, sub)
	d.mu.Unlock()

	// Replay history (no lock — these events are immutable in-memory copies).
	for _, ev := range historical {
		if err := yield(ev); err != nil {
			d.detachSubscriber(namespace, sub)
			return err
		}
	}
	_ = headCursor // documented invariant: live events have cursor > headCursor

	// Live loop.
	for {
		select {
		case <-ctx.Done():
			d.detachSubscriber(namespace, sub)
			return ctx.Err()
		case err := <-sub.err:
			return err
		case ev, ok := <-sub.ch:
			if !ok {
				return nil
			}
			if err := yield(ev); err != nil {
				d.detachSubscriber(namespace, sub)
				return err
			}
		}
	}
}

func (d *Driver) detachSubscriber(namespace string, sub *subscriber) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.streams[namespace]
	if s == nil {
		return
	}
	if sub.closed {
		return
	}
	sub.closed = true
	close(sub.ch)
	close(sub.err)
	for i, x := range s.subscribers {
		if x == sub {
			s.subscribers = append(s.subscribers[:i], s.subscribers[i+1:]...)
			return
		}
	}
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing means the host is broken; falling back to a
		// timestamp-only id is fine for an in-memory driver in that scenario.
		return time.Now().UTC().Format("20060102T150405.000000000")
	}
	// RFC 4122 v4 fixed bits.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[0:4]) + "-" + hex.EncodeToString(b[4:6]) + "-" +
		hex.EncodeToString(b[6:8]) + "-" + hex.EncodeToString(b[8:10]) + "-" +
		hex.EncodeToString(b[10:16])
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

func cloneMeta(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
