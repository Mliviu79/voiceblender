package api

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/recording"
	"github.com/VoiceBlender/voiceblender/internal/storage"
)

// TestPipeReader_TryRead covers the non-blocking read contract: nothing ready
// yields (0, nil) without waiting, a buffered remainder is served before the
// channel, and io.EOF appears only after Close plus a full drain.
func TestPipeReader_TryRead(t *testing.T) {
	t.Run("empty returns zero without blocking", func(t *testing.T) {
		pr, _ := createPipe()
		p := make([]byte, 4)
		n, err := pr.TryRead(p)
		if n != 0 || err != nil {
			t.Fatalf("TryRead on empty pipe = (%d, %v), want (0, nil)", n, err)
		}
	})

	t.Run("serves a queued frame", func(t *testing.T) {
		pr, pw := createPipe()
		pw.Write([]byte{1, 2, 3, 4})
		p := make([]byte, 4)
		n, err := pr.TryRead(p)
		if n != 4 || err != nil {
			t.Fatalf("TryRead = (%d, %v), want (4, nil)", n, err)
		}
		if string(p) != string([]byte{1, 2, 3, 4}) {
			t.Fatalf("TryRead payload = %v, want [1 2 3 4]", p)
		}
	})

	t.Run("serves buffered remainder before the channel", func(t *testing.T) {
		pr, pw := createPipe()
		pw.Write([]byte{1, 2, 3, 4})
		pw.Write([]byte{9, 9})

		// A short read leaves a remainder buffered on the reader.
		small := make([]byte, 2)
		n, err := pr.TryRead(small)
		if n != 2 || err != nil {
			t.Fatalf("first TryRead = (%d, %v), want (2, nil)", n, err)
		}
		if small[0] != 1 || small[1] != 2 {
			t.Fatalf("first TryRead payload = %v, want [1 2]", small)
		}

		// The remainder of frame one must win over the queued frame two.
		n, err = pr.TryRead(small)
		if n != 2 || err != nil {
			t.Fatalf("second TryRead = (%d, %v), want (2, nil)", n, err)
		}
		if small[0] != 3 || small[1] != 4 {
			t.Fatalf("remainder served out of order: got %v, want [3 4]", small)
		}

		n, err = pr.TryRead(small)
		if n != 2 || err != nil {
			t.Fatalf("third TryRead = (%d, %v), want (2, nil)", n, err)
		}
		if small[0] != 9 || small[1] != 9 {
			t.Fatalf("third TryRead payload = %v, want [9 9]", small)
		}
	})

	t.Run("EOF only after close and drain", func(t *testing.T) {
		pr, pw := createPipe()
		pw.Write([]byte{7, 7})
		pw.Close()

		// Still queued data: must be served, not swallowed by the close.
		p := make([]byte, 4)
		n, err := pr.TryRead(p)
		if n != 2 || err != nil {
			t.Fatalf("TryRead after Close with data queued = (%d, %v), want (2, nil)", n, err)
		}

		n, err = pr.TryRead(p)
		if n != 0 || err != io.EOF {
			t.Fatalf("TryRead after Close and drain = (%d, %v), want (0, io.EOF)", n, err)
		}
	})

	t.Run("open and empty is not EOF", func(t *testing.T) {
		pr, _ := createPipe()
		p := make([]byte, 4)
		for i := 0; i < 3; i++ {
			n, err := pr.TryRead(p)
			if n != 0 || err != nil {
				t.Fatalf("TryRead on open empty pipe = (%d, %v), want (0, nil)", n, err)
			}
		}
	})
}

// TestStartStereo_CompanionAudioReachesLeftChannel wires the real recording
// pipes into the real stereo recorder, which is the linkage the recording
// package's own tests cannot cover: they stand in their own pipe double, so
// they would stay green if this pipe stopped satisfying the recorder's
// non-blocking-read requirement and every recording's left channel silently
// went mute.
func TestStartStereo_CompanionAudioReachesLeftChannel(t *testing.T) {
	const (
		rate         = 8000
		slotBytes    = rate / 50 * 2 // one 20 ms frame, as the taps emit
		nFrames      = 8
		masterSample = int16(0x2222)
		compSample   = int16(0x1111)
	)

	frameOf := func(v int16) []byte {
		b := make([]byte, slotBytes)
		for i := 0; i+1 < len(b); i += 2 {
			binary.LittleEndian.PutUint16(b[i:], uint16(v))
		}
		return b
	}

	dir := t.TempDir()
	// Exactly the wiring doStartRecordLeg uses for a standalone SIP leg.
	leftPR, leftPW := createPipe()
	rightPR, rightPW := createPipe()

	rec := recording.NewRecorder(slog.Default())
	fpath, err := rec.StartStereo(context.Background(), leftPR, rightPR, dir, rate)
	if err != nil {
		t.Fatalf("StartStereo: %v", err)
	}

	for k := 0; k < nFrames; k++ {
		leftPW.Write(frameOf(compSample))
		rightPW.Write(frameOf(masterSample))
	}
	// Let the recorder drain before the close races the queued frames.
	time.Sleep(200 * time.Millisecond)
	leftPW.Close()
	rightPW.Close()
	rec.Wait()

	data, err := os.ReadFile(fpath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) < 44 {
		t.Fatalf("WAV is %d bytes: the recorder wrote nothing, so it rejected these pipes", len(data))
	}
	pcm := data[44:]

	var sawMaster, sawCompanion bool
	for i := 0; i+3 < len(pcm); i += 4 {
		if int16(binary.LittleEndian.Uint16(pcm[i:])) == compSample {
			sawCompanion = true
		}
		if int16(binary.LittleEndian.Uint16(pcm[i+2:])) == masterSample {
			sawMaster = true
		}
	}
	if !sawMaster {
		t.Error("right channel carries no master audio: the paced pipe is not clocking the recorder")
	}
	if !sawCompanion {
		t.Error("left channel is silent: audio written to the companion pipe never reached the recording")
	}
}

// The per-request S3 backend must inherit the operator's insecure-endpoint
// decision from server config — a caller must not be able to downgrade the
// transport, and an operator who has opted in must not be blocked. Both call
// sites build storage.S3Config as a named-field literal, so dropping the field
// still compiles: this test is what catches that.
func TestResolveStorage_AllowInsecure(t *testing.T) {
	// httptest serves http://127.0.0.1:..., i.e. a genuinely plaintext
	// endpoint, which is exactly the condition under test.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead && r.URL.Path == "/test-bucket" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	req := RecordRequest{
		Storage:     "s3",
		S3Bucket:    "test-bucket",
		S3Endpoint:  srv.URL,
		S3AccessKey: "key",
		S3SecretKey: "secret",
	}

	t.Run("operator opted in", func(t *testing.T) {
		s := &Server{Config: config.Config{S3AllowInsecureEndpoint: true}}

		backend, err := s.resolveStorage(req)
		if errors.Is(err, storage.ErrInsecureEndpoint) {
			t.Fatalf("S3_ALLOW_INSECURE_ENDPOINT=true must reach the endpoint, got %v", err)
		}
		if err != nil {
			t.Fatalf("expected the backend to be created, got %v", err)
		}
		if backend == nil {
			t.Fatal("expected a non-nil backend")
		}
	})

	t.Run("insecure endpoint refused by default", func(t *testing.T) {
		s := &Server{Config: config.Config{S3AllowInsecureEndpoint: false}}

		_, err := s.resolveStorage(req)
		if !errors.Is(err, storage.ErrInsecureEndpoint) {
			t.Fatalf("expected ErrInsecureEndpoint (surfaced to the caller as 400), got %v", err)
		}
	})
}
