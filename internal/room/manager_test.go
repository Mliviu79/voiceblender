package room

import (
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
)

// TestManager_DeletePanickingLegDoesNotCrash verifies that a panic inside
// one leg's Hangup during the room-delete fan-out is recovered rather than
// crashing the process: Delete still returns (wg completes), the room is
// removed from the map, and RoomDeleted still publishes.
func TestManager_DeletePanickingLegDoesNotCrash(t *testing.T) {
	bus := newTestBus()
	var eventTypes []events.EventType
	bus.Subscribe(func(e events.Event) {
		eventTypes = append(eventTypes, e.Type)
	})

	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, bus, newTestLog())

	r, err := mgr.Create("r1", "", 0)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	good := newMockLeg("good")
	bad := newMockLeg("bad")
	bad.panicOnHangup = true
	r.AddLeg(good)
	r.AddLeg(bad)

	if err := mgr.Delete("r1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, ok := mgr.Get("r1"); ok {
		t.Error("room should be deleted")
	}

	found := false
	for _, et := range eventTypes {
		if et == events.RoomDeleted {
			found = true
		}
	}
	if !found {
		t.Error("expected room.deleted event after a panicking leg hangup")
	}
}
