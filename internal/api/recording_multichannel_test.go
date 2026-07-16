package api

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/recording"
)

// fakeMixer stands in for the room mixer, which stopAll only ever uses to
// detach record taps.
type fakeMixer struct{}

func (fakeMixer) SetParticipantRecordTap(string, io.Writer) {}
func (fakeMixer) ClearParticipantRecordTap(string)          {}

// TestMultiChannelStopAll_SurvivesDiscardedLeg pins the blast radius of a single
// leg's capture failure. A discarded capture leaves nothing at the path Stop
// reports, and the merge refuses to open a file that is not there — so storing
// that path anyway meant one participant's failure destroyed every other
// participant's audio along with it. The healthy legs must still merge, and the
// dropped one must be named rather than silently vanishing.
func TestMultiChannelStopAll_SurvivesDiscardedLeg(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	mc := &multiChannelState{
		active: true,
		// Backdated so the merge spans a real duration and actually writes frames.
		startTime:    time.Now().Add(-time.Second),
		sampleRate:   8000,
		dir:          dir,
		recorders:    map[string]*recording.Recorder{},
		pipes:        map[string]*pipeWriter{},
		files:        map[string]string{},
		joinOffsets:  map[string]time.Duration{},
		leaveOffsets: map[string]time.Duration{},
		log:          slog.Default(),
	}

	// A healthy leg: a finite reader, so the capture ends at EOF and publishes.
	// Waiting for it here is what makes the test deterministic — stopAll would
	// otherwise cancel it mid-flight, and a capture cancelled before its first
	// frame publishes a headerless file the merge rejects for an unrelated
	// reason.
	good := recording.NewRecorder(slog.Default())
	if _, err := good.StartAt(ctx, bytes.NewReader(make([]byte, 16000)), dir, 8000); err != nil {
		t.Fatalf("start good leg: %v", err)
	}
	good.Wait()
	if !good.Published() {
		t.Fatal("precondition: the good leg did not publish, so this test proves nothing")
	}

	// A failing leg: a stereo companion that cannot be drained without blocking
	// is a real, reachable capture error, so this staging file is discarded.
	bad := recording.NewRecorder(slog.Default())
	if _, err := bad.StartStereo(ctx, bytes.NewReader(nil), bytes.NewReader(nil), dir, 8000); err != nil {
		t.Fatalf("start bad leg: %v", err)
	}
	bad.Wait()
	if bad.Published() {
		t.Fatal("precondition: the bad leg published, so this test proves nothing")
	}

	mc.recorders["good"] = good
	mc.recorders["bad"] = bad
	mc.participantOrder = []string{"good", "bad"}

	res, err := mc.stopAll(fakeMixer{})
	if err != nil {
		t.Fatalf("stopAll lost the whole room over one discarded leg: %v", err)
	}
	if _, ok := res.Channels["good"]; !ok {
		t.Errorf("the healthy leg is missing from the merge; channels = %v", res.Channels)
	}
	if _, ok := res.Channels["bad"]; ok {
		t.Errorf("the discarded leg was given a channel, but it has no audio; channels = %v", res.Channels)
	}
	if got := res.OmittedLegs; len(got) != 1 || got[0] != "bad" {
		t.Errorf("OmittedLegs = %v, want [bad] — a partial recording must name what it lost", got)
	}
}

// TestMultiChannelStopAll_AllLegsDiscardedFails is the other half of the
// contract: salvaging survivors must not degrade into reporting an empty room as
// a success. With nothing to merge, the stop has to fail loudly.
func TestMultiChannelStopAll_AllLegsDiscardedFails(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	mc := &multiChannelState{
		active:       true,
		startTime:    time.Now().Add(-time.Second),
		sampleRate:   8000,
		dir:          dir,
		recorders:    map[string]*recording.Recorder{},
		pipes:        map[string]*pipeWriter{},
		files:        map[string]string{},
		joinOffsets:  map[string]time.Duration{},
		leaveOffsets: map[string]time.Duration{},
		log:          slog.Default(),
	}

	bad := recording.NewRecorder(slog.Default())
	if _, err := bad.StartStereo(ctx, bytes.NewReader(nil), bytes.NewReader(nil), dir, 8000); err != nil {
		t.Fatalf("start bad leg: %v", err)
	}
	bad.Wait()
	if bad.Published() {
		t.Fatal("precondition: the bad leg published, so this test proves nothing")
	}

	mc.recorders["bad"] = bad
	mc.participantOrder = []string{"bad"}

	if _, err := mc.stopAll(fakeMixer{}); err == nil {
		t.Fatal("stopAll reported success though no leg published any audio")
	}
}
