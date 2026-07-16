package api

import (
	"io"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
)

// panicReader panics on its first Read, standing in for a leg whose inbound
// audio path blows up inside the mixer's readLoop.
type panicReader struct{}

func (panicReader) Read(p []byte) (int, error) { panic("simulated read panic") }

// panicAudioLeg is an apiMockLeg with a real, panicking audio path.
// apiMockLeg hardcodes AudioReader() to nil, which the mixer never reads from,
// so the overrides are what let the panic come through the real mixer — the
// whole point of the test.
type panicAudioLeg struct {
	*apiMockLeg
}

func (p *panicAudioLeg) AudioReader() io.Reader { return panicReader{} }
func (p *panicAudioLeg) AudioWriter() io.Writer { return io.Discard }
func (p *panicAudioLeg) SampleRate() int        { return 16000 }

// TestPanickedLegPublishesDisconnect drives a mixer IO panic through the real
// wiring NewServer installs — real leg.Manager, real room.Manager, real bus —
// and asserts the API layer finishes the teardown the room layer started.
//
// Before the callback existed, such a leg emitted leg.left_room and nothing
// else: no CDR, an unended span, and it stayed in the leg manager forever, so
// GET /v1/legs/{id} kept serving a dead leg.
func TestPanickedLegPublishesDisconnect(t *testing.T) {
	s := newTestServer(t)

	var mu sync.Mutex
	var gotReason string
	var gotDisconnect bool
	s.Bus.Subscribe(func(e events.Event) {
		if e.Type != events.LegDisconnected {
			return
		}
		d, ok := e.Data.(*events.LegDisconnectedData)
		if !ok || d.LegID != "panic-leg" {
			return
		}
		mu.Lock()
		gotDisconnect = true
		gotReason = d.CDR.Reason
		mu.Unlock()
	})

	l := &panicAudioLeg{apiMockLeg: &apiMockLeg{id: "panic-leg", createdAt: time.Now()}}
	s.LegMgr.Add(l)
	if _, err := s.RoomMgr.Create("r1", "", 0); err != nil {
		t.Fatalf("Create room: %v", err)
	}
	if err := s.RoomMgr.AddLeg("r1", "panic-leg"); err != nil {
		t.Fatalf("AddLeg: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		done := gotDisconnect
		mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no leg.disconnected published for a leg killed by a mixer IO panic")
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	reason := gotReason
	mu.Unlock()
	if reason != "mixer_panic" {
		t.Fatalf("cdr.reason = %q, want %q", reason, "mixer_panic")
	}

	// cleanupLeg must have run too, or the leg leaks in the leg manager and
	// GET /v1/legs/{id} keeps serving it.
	if _, ok := s.LegMgr.Get("panic-leg"); ok {
		t.Fatal("panicked leg still registered in the leg manager")
	}
}
