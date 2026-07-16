package recording

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-audio/wav"
)

// stagingFiles lists the staging files sitting in dir.
func stagingFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, stagingPattern))
	if err != nil {
		t.Fatalf("glob staging files in %s: %v", dir, err)
	}
	return m
}

// assertNoStagingResidue fails if a staging file outlived the recording.
func assertNoStagingResidue(t *testing.T, dir string) {
	t.Helper()
	if got := stagingFiles(t, dir); len(got) != 0 {
		t.Errorf("staging residue left behind in %s: %v", dir, got)
	}
}

// assertPublishedMode fails unless path carries the mode recordings have always
// been published with. Staging files are opened 0600 and a rename keeps the
// inode's mode, so a recording published without an explicit chmod would
// silently become owner-only and lock out consumers running as another user.
func assertPublishedMode(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := fi.Mode().Perm(); got != recordingFileMode {
		t.Errorf("%s published with mode %v, want %v", path, got, os.FileMode(recordingFileMode))
	}
}

// assertPlayable fails unless path holds a WAV the standard decoder accepts,
// which is what proves the size header was rewritten before publishing.
func assertPlayable(t *testing.T, path string, wantChannels int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	dec := wav.NewDecoder(f)
	if !dec.IsValidFile() {
		t.Fatalf("%s is not a valid WAV — it was published before its header was final", path)
	}
	if got := int(dec.NumChans); got != wantChannels {
		t.Errorf("%s has %d channels, want %d", path, got, wantChannels)
	}
	buf, err := dec.FullPCMBuffer()
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if len(buf.Data) == 0 {
		t.Errorf("%s decoded to no audio", path)
	}
}

func TestPublishFile(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "publish.wav")

	staged, err := createStagedFile(final)
	if err != nil {
		t.Fatalf("createStagedFile: %v", err)
	}
	want := []byte("durable bytes")
	if _, err := staged.f.Write(want); err != nil {
		t.Fatalf("write staged: %v", err)
	}

	// Nothing is at the final name until it is published.
	if _, err := os.Stat(final); !os.IsNotExist(err) {
		t.Fatalf("%s exists before publish, os.Stat err = %v", final, err)
	}

	if err := publishFile(staged.f, staged.tmpPath, staged.finalPath); err != nil {
		t.Fatalf("publishFile: %v", err)
	}

	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("read published file: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("published content = %q, want %q", got, want)
	}
	if _, err := os.Stat(staged.tmpPath); !os.IsNotExist(err) {
		t.Errorf("staging file %s survived publish, os.Stat err = %v", staged.tmpPath, err)
	}
	assertPublishedMode(t, final)
	assertNoStagingResidue(t, dir)
}

func TestPublishFile_RenameFailureLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	staged, err := createStagedFile(filepath.Join(dir, "unreachable.wav"))
	if err != nil {
		t.Fatalf("createStagedFile: %v", err)
	}
	if _, err := staged.f.Write([]byte("bytes")); err != nil {
		t.Fatalf("write staged: %v", err)
	}

	// A directory that does not exist cannot be renamed into.
	bad := filepath.Join(dir, "absent", "publish.wav")
	if err := publishFile(staged.f, staged.tmpPath, bad); err == nil {
		t.Fatal("publishFile succeeded renaming into a missing directory, want error")
	}
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Errorf("%s exists after a failed publish, os.Stat err = %v", bad, err)
	}
	assertNoStagingResidue(t, dir)
}

func TestDiscardTemp(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "discarded.wav")

	staged, err := createStagedFile(final)
	if err != nil {
		t.Fatalf("createStagedFile: %v", err)
	}
	if _, err := staged.f.Write([]byte("bytes that must not surface")); err != nil {
		t.Fatalf("write staged: %v", err)
	}

	discardTemp(staged.f, staged.tmpPath)

	if _, err := os.Stat(final); !os.IsNotExist(err) {
		t.Errorf("%s exists after discard, os.Stat err = %v", final, err)
	}
	if _, err := os.Stat(staged.tmpPath); !os.IsNotExist(err) {
		t.Errorf("staging file %s survived discard, os.Stat err = %v", staged.tmpPath, err)
	}
	assertNoStagingResidue(t, dir)
}

func TestSyncDir(t *testing.T) {
	if err := syncDir(t.TempDir()); err != nil {
		t.Errorf("syncDir: %v", err)
	}
	if err := syncDir(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("syncDir on a missing directory succeeded, want error")
	}
}

// frameGate wraps the recorder's input and reports when the recorder has come
// back for a second read. That is the moment the first frame has provably been
// through enc.Write, so the mid-flight assertions below describe a recording
// that is genuinely capturing audio — established without a sleep, which would
// make the test both flaky and vacuous.
//
// It is read only by the recording goroutine; the test only ever receives on
// secondRead.
type frameGate struct {
	r          *syncPipeReader
	reads      int
	secondRead chan struct{}
}

func (g *frameGate) Read(p []byte) (int, error) {
	g.reads++
	if g.reads == 2 {
		close(g.secondRead)
	}
	return g.r.Read(p)
}

// TestRecorder_StartStop_PublishesFinalOnly is the headline guard: a recording
// in progress must exist only as a staging file, and its published name must
// stay absent until the recording stops. Without that, anything listing the
// directory mid-call can pick up a WAV whose size header has not been written
// yet.
//
// The mid-flight assertion is the load-bearing one. The post-stop state is
// identical whether or not the recording was staged, so post-stop checks alone
// would pass against a recorder that wrote straight to its final name.
func TestRecorder_StartStop_PublishesFinalOnly(t *testing.T) {
	const sampleRate = 8000

	dir := t.TempDir()
	r := NewRecorder(slog.Default())

	pr, pw := newSyncPipe()
	gate := &frameGate{r: pr, secondRead: make(chan struct{})}

	fpath, err := r.StartAt(context.Background(), gate, dir, sampleRate)
	if err != nil {
		t.Fatalf("StartAt: %v", err)
	}

	// One 20 ms frame of audible audio.
	frame := make([]byte, 640)
	for i := range frame {
		frame[i] = 0x11
	}
	if _, err := pw.Write(frame); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Mid-flight: the frame is encoded and the recorder is parked on the next read.
	<-gate.secondRead

	if got := stagingFiles(t, dir); len(got) != 1 {
		t.Fatalf("mid-recording: found %d staging files in %s (%v), want exactly 1 — the recording is not being staged", len(got), dir, got)
	}
	if _, err := os.Stat(fpath); !os.IsNotExist(err) {
		t.Fatalf("mid-recording: %s already exists, os.Stat err = %v — a partial recording is visible at its published name", fpath, err)
	}
	if r.Published() {
		t.Error("mid-recording: Published() is true before the recording stopped")
	}

	pw.Close()
	if got := r.Stop(); got != fpath {
		t.Errorf("Stop returned %q, want the path StartAt returned, %q", got, fpath)
	}
	r.Wait()

	// Post-stop: the recording is at its published name, whole, and nothing else is left.
	assertPlayable(t, fpath, 1)
	assertPublishedMode(t, fpath)
	assertNoStagingResidue(t, dir)
	if !r.Published() {
		t.Error("Published() is false after a recording that stopped normally")
	}
}

// TestRecorder_CaptureError_DiscardsStagedFile covers the discard branch: a
// capture that fails must leave nothing at its published name.
//
// The error is raised by handing recordStereo a companion it cannot drain
// without blocking, which is a real, reachable capture failure. A true
// enc.Write failure needs the underlying file write to fail and is not
// practically inducible here; it reaches this same branch, which is also
// covered directly by TestDiscardTemp.
func TestRecorder_CaptureError_DiscardsStagedFile(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(slog.Default())

	fpath, err := r.StartStereo(context.Background(), &blockingReader{}, &blockingReader{}, dir, 8000)
	if err != nil {
		t.Fatalf("StartStereo: %v", err)
	}
	r.Wait()

	if _, err := os.Stat(fpath); !os.IsNotExist(err) {
		t.Errorf("%s exists after a failed capture, os.Stat err = %v", fpath, err)
	}
	if r.Published() {
		t.Error("Published() is true after a capture that failed and was discarded")
	}
	assertNoStagingResidue(t, dir)
}

// eofReader yields no data and ends immediately, so a capture loop reading it
// writes nothing and sees no error of its own.
type eofReader struct{}

func (eofReader) Read(p []byte) (int, error) { return 0, io.EOF }

// TestRecorder_CloseErrorIsCaptureFailure pins the one capture failure the loop
// itself cannot see. go-audio's Encoder.Close is the sole writer of the real
// RIFF/data sizes, so if it fails the file keeps its placeholder header and is
// not a playable WAV — yet every write the loop made succeeded. Closing the fd
// out from under the encoder reproduces exactly that: the reader ends at once so
// enc.Write never runs, and Close's header rewrite is the only thing that fails.
// Unless that error is surfaced, finish() publishes an unreadable recording and
// reports Published()==true for it.
func TestRecorder_CloseErrorIsCaptureFailure(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "closed.wav"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Close the fd so the encoder's trailing header rewrite cannot land.
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	r := NewRecorder(slog.Default())
	if err := r.recordMono(context.Background(), eofReader{}, f, 8000); err == nil {
		t.Fatal("recordMono reported success though the WAV size header could not be rewritten")
	}
}

// TestRecorder_NormalStopPublishes pins the finalize trigger: Stop cancels the
// recording's context, and a cancelled context is the normal end of a
// recording, not a failure. Treating it as one would discard every recording
// that was stopped the ordinary way.
func TestRecorder_NormalStopPublishes(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(slog.Default())

	pr, pw := newSyncPipe()
	gate := &frameGate{r: pr, secondRead: make(chan struct{})}

	fpath, err := r.StartAt(context.Background(), gate, dir, 8000)
	if err != nil {
		t.Fatalf("StartAt: %v", err)
	}
	if _, err := pw.Write(make([]byte, 640)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	<-gate.secondRead

	// Stop without ever closing the reader: the context cancel alone ends it.
	r.Stop()
	pw.Close()
	r.Wait()

	if !r.Published() {
		t.Fatal("Published() is false after Stop — a normally stopped recording was discarded")
	}
	assertPlayable(t, fpath, 1)
	assertNoStagingResidue(t, dir)
}
