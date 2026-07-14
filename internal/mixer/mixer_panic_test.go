package mixer

import (
	"io"
	"log/slog"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// panicAfterReader panics on the (limit+1)th call to Read; before that it
// returns a fixed frame. Used to simulate a participant whose Reader.Read
// panics on a malformed frame after N good reads.
type panicAfterReader struct {
	n     int32
	limit int32
	frame []byte
}

func (r *panicAfterReader) Read(p []byte) (int, error) {
	if atomic.AddInt32(&r.n, 1) > r.limit {
		panic("simulated read panic")
	}
	copy(p, r.frame)
	return len(p), nil
}

// silenceReader always returns a fixed (silent) frame, never panics. Used as
// the "other side" of a participant whose Writer is the one under test.
type silenceReader struct {
	frame []byte
}

func (r *silenceReader) Read(p []byte) (int, error) {
	copy(p, r.frame)
	return len(p), nil
}

// panicAfterWriter panics on the (limit+1)th call to Write; before that it
// accepts the write silently.
type panicAfterWriter struct {
	n     int32
	limit int32
}

func (w *panicAfterWriter) Write(p []byte) (int, error) {
	if atomic.AddInt32(&w.n, 1) > w.limit {
		panic("simulated write panic")
	}
	return len(p), nil
}

// panicOnceWriter panics on its first Write call and behaves normally
// afterward. Used to force exactly one bad mixTick.
type panicOnceWriter struct {
	fired atomic.Bool
}

func (w *panicOnceWriter) Write(p []byte) (int, error) {
	if w.fired.CompareAndSwap(false, true) {
		panic("simulated tap panic")
	}
	return len(p), nil
}

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// waitFor polls cond until it's true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", msg)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestMixer_ReadLoopPanicRemovesParticipant verifies that a panic inside a
// participant's Reader.Read is recovered, removes exactly that participant,
// and does not take down the mixer — other participants keep receiving
// mixed output afterward.
func TestMixer_ReadLoopPanicRemovesParticipant(t *testing.T) {
	m := New(testLog(), DefaultSampleRate)
	m.Start()
	defer m.Stop()

	fsz := m.frameSizeBytes

	victimReader := &panicAfterReader{limit: 2, frame: make([]byte, fsz)}
	m.AddParticipant("victim", victimReader, io.Discard)

	survivorReader, survivorFeeder := io.Pipe()
	survivorCapture := &captureWriter{}
	m.AddParticipant("survivor", survivorReader, survivorCapture)
	stopFeed := make(chan struct{})
	defer close(stopFeed)
	go func() {
		silence := make([]byte, fsz)
		ticker := time.NewTicker(time.Duration(Ptime) * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopFeed:
				return
			case <-ticker.C:
				if _, err := survivorFeeder.Write(silence); err != nil {
					return
				}
			}
		}
	}()
	defer survivorFeeder.Close()

	waitFor(t, 2*time.Second, "victim removed after read panic", func() bool {
		return m.ParticipantCount() == 1
	})

	before := len(survivorCapture.Bytes())
	time.Sleep(200 * time.Millisecond)
	after := len(survivorCapture.Bytes())
	if after <= before {
		t.Fatalf("survivor stopped receiving audio after victim's read panic: before=%d after=%d", before, after)
	}
}

// TestMixer_WriteLoopPanicRemovesParticipant verifies that a panic inside a
// participant's Writer.Write is recovered, removes exactly that
// participant, and the mixer keeps ticking for the rest.
func TestMixer_WriteLoopPanicRemovesParticipant(t *testing.T) {
	m := New(testLog(), DefaultSampleRate)
	m.Start()
	defer m.Stop()

	fsz := m.frameSizeBytes

	victimReader := &silenceReader{frame: make([]byte, fsz)}
	victimWriter := &panicAfterWriter{limit: 2}
	m.AddParticipant("victim", victimReader, victimWriter)

	survivorReader, survivorFeeder := io.Pipe()
	survivorCapture := &captureWriter{}
	m.AddParticipant("survivor", survivorReader, survivorCapture)
	stopFeed := make(chan struct{})
	defer close(stopFeed)
	go func() {
		silence := make([]byte, fsz)
		ticker := time.NewTicker(time.Duration(Ptime) * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopFeed:
				return
			case <-ticker.C:
				if _, err := survivorFeeder.Write(silence); err != nil {
					return
				}
			}
		}
	}()
	defer survivorFeeder.Close()

	waitFor(t, 2*time.Second, "victim removed after write panic", func() bool {
		return m.ParticipantCount() == 1
	})

	before := len(survivorCapture.Bytes())
	time.Sleep(200 * time.Millisecond)
	after := len(survivorCapture.Bytes())
	if after <= before {
		t.Fatalf("survivor stopped receiving audio after victim's write panic: before=%d after=%d", before, after)
	}
}

// TestMixer_MixTickPanicSkipsTickNotRoom is the load-bearing assertion that
// mixTick is recovered per-tick (via safeMixTick), not by a defer wrapped
// around mixLoop. It drives safeMixTick directly: a panic on the first call
// must be swallowed with no output produced for that tick, and the very
// next call must complete normally and produce output — proving the mix
// loop itself was never unwound.
func TestMixer_MixTickPanicSkipsTickNotRoom(t *testing.T) {
	m := New(testLog(), DefaultSampleRate)

	fsz := m.frameSizeBytes
	spf := m.samplesPerFrame

	gw := &guardedWriter{w: io.Discard}
	p := &Participant{
		ID:       "listener",
		Writer:   gw,
		incoming: make(chan []byte, 3),
		outgoing: make(chan []byte, 3),
		inject:   make(chan []byte, 3),
		done:     make(chan struct{}),
		guard:    gw,
	}
	// tap.Write is called early in mixTick, before the per-listener mix loop
	// that produces output — so a panic here proves the whole tick is
	// skipped, not just this participant's slice of it.
	panicTap := &panicOnceWriter{}
	p.tap = panicTap

	m.mu.Lock()
	m.participants["listener"] = p
	m.mu.Unlock()

	frame := make([]byte, fsz)

	// Tick 1: tap panics. safeMixTick must recover; no output for this tick.
	p.incoming <- frame
	m.safeMixTick()

	select {
	case <-p.outgoing:
		t.Fatal("expected no output on the panicking tick")
	default:
	}

	// Tick 2: tap no longer panics (fired once); mixTick must complete
	// normally and produce output — proving mixLoop's ticker case survived
	// the previous panic.
	p.incoming <- frame
	m.safeMixTick()

	select {
	case out := <-p.outgoing:
		if len(out) != spf*2 {
			t.Fatalf("unexpected output length = %d, want %d", len(out), spf*2)
		}
	default:
		t.Fatal("expected output on the tick after recovery")
	}
}

// TestMixer_ReadLoopPanicGoroutinesExit verifies that after a read-panic
// removes a participant, that participant's readLoop/writeLoop goroutines
// actually exit (via the close(p.done) path in RemoveParticipant) rather
// than leaking.
func TestMixer_ReadLoopPanicGoroutinesExit(t *testing.T) {
	m := New(testLog(), DefaultSampleRate)
	m.Start()
	defer m.Stop()

	fsz := m.frameSizeBytes

	// Let the mixer settle before taking the goroutine-count baseline.
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	victimReader := &panicAfterReader{limit: 1, frame: make([]byte, fsz)}
	m.AddParticipant("victim", victimReader, io.Discard)

	waitFor(t, 2*time.Second, "victim removed after read panic", func() bool {
		return m.ParticipantCount() == 0
	})

	waitFor(t, 2*time.Second, "victim's readLoop/writeLoop goroutines to exit", func() bool {
		return runtime.NumGoroutine() <= before
	})
}
