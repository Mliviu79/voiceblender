package mixer

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"testing"
)

// --- helpers ---------------------------------------------------------------

// tonePCM returns amp-scaled 16-bit LE PCM of a freq-Hz sine at rate, starting
// at sample offset so a stream can be produced in pieces without a phase jump.
func tonePCM(numSamples, offset int, freq float64, rate int, amp float64) []byte {
	out := make([]byte, numSamples*2)
	for i := range numSamples {
		v := amp * math.Sin(2*math.Pi*freq*float64(offset+i)/float64(rate))
		binary.LittleEndian.PutUint16(out[i*2:], uint16(int16(v*32767)))
	}
	return out
}

func decodePCM(p []byte) []int16 {
	out := make([]int16, len(p)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(p[i*2:]))
	}
	return out
}

// toneAmplitude returns the amplitude of the freq-Hz component of x, in units
// of full scale (a full-scale sine reads ~1.0).
func toneAmplitude(x []int16, freq float64, rate int) float64 {
	if len(x) == 0 {
		return 0
	}
	var re, im float64
	w := 2 * math.Pi * freq / float64(rate)
	for i, s := range x {
		v := float64(s) / 32768.0
		re += v * math.Cos(w*float64(i))
		im += v * math.Sin(w*float64(i))
	}
	return 2 * math.Hypot(re, im) / float64(len(x))
}

func rms(x []int16) float64 {
	if len(x) == 0 {
		return 0
	}
	var sum float64
	for _, s := range x {
		v := float64(s) / 32768.0
		sum += v * v
	}
	return math.Sqrt(sum / float64(len(x)))
}

// maxStep returns the largest sample-to-sample jump in x, in units of full scale.
func maxStep(x []int16) float64 {
	var m float64
	for i := 1; i < len(x); i++ {
		d := math.Abs(float64(x[i])-float64(x[i-1])) / 32768.0
		if d > m {
			m = d
		}
	}
	return m
}

// toneStep returns the largest sample-to-sample jump a clean freq-Hz sine of
// amplitude amp can produce at rate — the ceiling a continuous output must obey.
func toneStep(freq float64, rate int, amp float64) float64 {
	return amp * 2 * math.Pi * freq / float64(rate)
}

// seamTone is deliberately low so that toneStep is small: any zero-history gap
// re-injected at a chunk boundary then stands far above the tone's own slope.
const seamTone = 200.0

// assertContinuous asserts that out — a resampled continuous tone of amplitude
// amp at freq — carries the tone unbroken: full energy in every chunk (a
// resampler rebuilt per chunk emits its zero history instead, collapsing chunk
// RMS) and no step steeper than the tone itself can be.
func assertContinuous(t *testing.T, out []int16, chunk int, freq float64, rate int, amp float64) {
	t.Helper()

	// Skip the stream's own lead-in: the filter genuinely starts with zero
	// history exactly once, and that ramp is the accepted group delay.
	skip := 2 * chunk
	if len(out) <= skip+chunk {
		t.Fatalf("not enough output to measure: %d samples", len(out))
	}
	steady := out[skip:]

	wantRMS := amp / math.Sqrt2
	if got := rms(steady); got < wantRMS*0.95 {
		t.Errorf("steady-state RMS = %.4f, want >= %.4f (%.4f expected for a %g Hz sine at amplitude %g) — "+
			"the tone is being cut into, which is what a per-chunk resampler's zero history does",
			got, wantRMS*0.95, wantRMS, freq, amp)
	}

	// Every chunk must carry full energy. A resampler rebuilt per chunk fades
	// in over its group delay, so each chunk's leading samples collapse toward
	// zero while the overall waveform still looks plausible.
	for off := 0; off+chunk <= len(steady); off += chunk {
		if got := rms(steady[off : off+chunk]); got < wantRMS*0.9 {
			t.Errorf("chunk at sample %d has RMS %.4f, want >= %.4f — filter history is not carrying across the chunk boundary",
				skip+off, got, wantRMS*0.9)
			break
		}
	}

	limit := 2 * toneStep(freq, rate, amp)
	if got := maxStep(steady); got > limit {
		t.Errorf("max sample-to-sample step = %.4f, want <= %.4f — the output has a discontinuity a %g Hz sine cannot produce",
			got, limit, freq)
	}
}

// --- passthrough -----------------------------------------------------------

func TestResample_Passthrough(t *testing.T) {
	if r := NewPCMResampler(16000, 16000); r != nil {
		t.Errorf("NewPCMResampler at equal rates allocated a filter: %#v", r)
	}

	src := bytes.NewReader(nil)
	if got := NewResampleReader(src, 8000, 8000); got != io.Reader(src) {
		t.Errorf("NewResampleReader at equal rates = %#v, want the wrapped reader unchanged", got)
	}
	var dst bytes.Buffer
	if got := NewResampleWriter(&dst, 8000, 8000); got != io.Writer(&dst) {
		t.Errorf("NewResampleWriter at equal rates = %#v, want the wrapped writer unchanged", got)
	}

	// A nil resampler is a passthrough, byte for byte.
	in := tonePCM(160, 0, 440, 8000, 0.8)
	var nilRS *PCMResampler
	if got := nilRS.ResampleBytes(in); !bytes.Equal(got, in) {
		t.Error("nil PCMResampler.ResampleBytes did not return the input unchanged")
	}
}

// --- attenuation -----------------------------------------------------------

// TestResampleReader_Attenuation feeds a 6 kHz tone through a 16 kHz -> 8 kHz
// conversion. 6 kHz is above the 4 kHz destination Nyquist, so an unfiltered
// resampler folds it back to 2 kHz — squarely in the voice band. The
// anti-aliasing filter must remove it.
func TestResampleReader_Attenuation(t *testing.T) {
	const (
		srcRate, dstRate = 16000, 8000
		toneHz, aliasHz  = 6000.0, 2000.0
		amp              = 0.9
	)
	src := bytes.NewReader(tonePCM(srcRate/2, 0, toneHz, srcRate, amp)) // 500 ms
	r := NewResampleReader(src, srcRate, dstRate)

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	got := toneAmplitude(decodePCM(out), aliasHz, dstRate)
	// Two-tap linear interpolation leaves this alias at ~0.45 full scale.
	if got > 0.01 {
		t.Errorf("alias at %g Hz = %.5f full scale, want <= 0.01 — above-Nyquist energy is folding into the passband", aliasHz, got)
	}
}

func TestResampleWriter_Attenuation(t *testing.T) {
	const (
		srcRate, dstRate = 16000, 8000
		toneHz, aliasHz  = 6000.0, 2000.0
		amp              = 0.9
	)
	var dst bytes.Buffer
	w := NewResampleWriter(&dst, srcRate, dstRate)
	if _, err := w.Write(tonePCM(srcRate/2, 0, toneHz, srcRate, amp)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := toneAmplitude(decodePCM(dst.Bytes()), aliasHz, dstRate)
	if got > 0.01 {
		t.Errorf("alias at %g Hz = %.5f full scale, want <= 0.01 — above-Nyquist energy is folding into the passband", aliasHz, got)
	}
}

// --- duration mapping ------------------------------------------------------

// TestResample_DurationMapping pins the 1:1 time contract every caller depends
// on: N ms in, N ms out. It says nothing about phase — the filter's group delay
// shifts the waveform without changing the count, which is why
// TestResample_GroupDelay exists separately.
func TestResample_DurationMapping(t *testing.T) {
	cases := []struct{ src, dst int }{
		{8000, 16000},
		{16000, 8000},
		{44100, 8000},
		{8000, 48000},
		{48000, 16000},
	}
	const durMS = 500
	for _, c := range cases {
		t.Run(fmt.Sprintf("%d_to_%d", c.src, c.dst), func(t *testing.T) {
			inSamples := c.src * durMS / 1000
			want := c.dst * durMS / 1000
			// Two samples of slack absorbs the filter's fractional phase.
			const slack = 2

			in := tonePCM(inSamples, 0, 440, c.src, 0.5)

			out, err := io.ReadAll(NewResampleReader(bytes.NewReader(in), c.src, c.dst))
			if err != nil {
				t.Fatalf("reader ReadAll: %v", err)
			}
			if got := len(out) / 2; got < want-slack || got > want+slack {
				t.Errorf("reader produced %d samples, want %d (+/-%d)", got, want, slack)
			}

			var dst bytes.Buffer
			w := NewResampleWriter(&dst, c.src, c.dst)
			// Write in 20 ms frames: the real callers do, and it proves the
			// count contract holds per stream, not just per call.
			frame := c.src * 20 / 1000 * 2
			for off := 0; off < len(in); off += frame {
				end := min(off+frame, len(in))
				if _, err := w.Write(in[off:end]); err != nil {
					t.Fatalf("writer Write: %v", err)
				}
			}
			if got := dst.Len() / 2; got < want-slack || got > want+slack {
				t.Errorf("writer produced %d samples, want %d (+/-%d)", got, want, slack)
			}
		})
	}
}

// --- seam guards -----------------------------------------------------------

// TestResampleReader_NoSeamDiscontinuity drives a continuous sine through many
// small Reads. The filter is built once in NewResampleReader and its history
// carries across every Read; a resampler built inside Read would restart from
// zero history on each call and stamp that gap into the output at the chunk
// rate.
func TestResampleReader_NoSeamDiscontinuity(t *testing.T) {
	const (
		srcRate, dstRate = 16000, 8000
		amp              = 0.9
		frames           = 25
	)
	src := bytes.NewReader(tonePCM(srcRate*frames*20/1000, 0, seamTone, srcRate, amp))
	r := NewResampleReader(src, srcRate, dstRate)

	dstFrame := dstRate * 20 / 1000 // samples per 20 ms output frame
	var out []int16
	buf := make([]byte, dstFrame*2)
	for {
		n, err := io.ReadFull(r, buf)
		out = append(out, decodePCM(buf[:n])...)
		if err != nil {
			break
		}
	}
	assertContinuous(t, out, dstFrame, seamTone, dstRate, amp)
}

// TestResampleWriter_NoSeamDiscontinuity is the writer's own guard. The writer
// has the identical exposure to the reader and is the one that is live on the
// room bridge and the agent/STT feeds, so it cannot ride on the reader's test.
func TestResampleWriter_NoSeamDiscontinuity(t *testing.T) {
	const (
		srcRate, dstRate = 16000, 8000
		amp              = 0.9
		frames           = 25
	)
	var dst bytes.Buffer
	w := NewResampleWriter(&dst, srcRate, dstRate)

	srcFrame := srcRate * 20 / 1000
	for i := range frames {
		if _, err := w.Write(tonePCM(srcFrame, i*srcFrame, seamTone, srcRate, amp)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	assertContinuous(t, decodePCM(dst.Bytes()), dstRate*20/1000, seamTone, dstRate, amp)
}

// --- group delay -----------------------------------------------------------

// The anti-aliasing filter adds group delay. Filtering above-Nyquist energy
// requires a filter and a filter has group delay, so this is the accepted price
// of the conversion, not a defect — but it is a budget, and a rate-crossing
// call pays it twice because the room bridge wraps both directions.
//
// These bounds bracket resamplerQuality = 4 (measured 4.00 ms per conversion,
// 8 ms round-trip) tightly enough to fail if the quality moves: quality 3 costs
// 3.00 ms, quality 6 costs 6.00 ms, quality 8 costs 10.00 ms. Changing
// resamplerQuality is meant to fail here and be re-accepted, not to slip
// through.
const (
	groupDelayMinMS = 3.5
	groupDelayMaxMS = 5.0
)

func TestResample_GroupDelay(t *testing.T) {
	cases := []struct{ src, dst int }{
		{8000, 16000},
		{16000, 8000},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%d_to_%d", c.src, c.dst), func(t *testing.T) {
			r := NewPCMResampler(c.src, c.dst)

			delayMS := float64(r.OutputLatency()) / float64(c.dst) * 1000
			if delayMS < groupDelayMinMS || delayMS > groupDelayMaxMS {
				t.Errorf("filter group delay = %.2f ms (%d samples at %d Hz), budget is %.1f-%.1f ms",
					delayMS, r.OutputLatency(), c.dst, groupDelayMinMS, groupDelayMaxMS)
			}

			// Corroborate the reported latency against the waveform: a
			// silence -> tone step must come out shifted by that much.
			const (
				silenceMS = 100
				toneMS    = 200
				toneHz    = 500.0
				amp       = 0.9
			)
			in := make([]byte, c.src*silenceMS/1000*2)
			in = append(in, tonePCM(c.src*toneMS/1000, 0, toneHz, c.src, amp)...)

			out, err := io.ReadAll(NewResampleReader(bytes.NewReader(in), c.src, c.dst))
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			samples := decodePCM(out)

			// Onset = first sample past a tenth of full amplitude.
			threshold := int16(math.Round(amp * 0.1 * 32767))
			onset := -1
			for i, s := range samples {
				if s > threshold || s < -threshold {
					onset = i
					break
				}
			}
			if onset < 0 {
				t.Fatal("no tone onset found in the output")
			}
			// Measures +4.06 ms (8k->16k) and +4.12 ms (16k->8k) against the
			// 4.00 ms OutputLatency reports, so 1 ms either side of the budget
			// is ample for the threshold crossing while still failing if the
			// delay itself moves.
			shiftMS := float64(onset)/float64(c.dst)*1000 - silenceMS
			if shiftMS < groupDelayMinMS-1 || shiftMS > groupDelayMaxMS+1 {
				t.Errorf("measured onset shift = %+.2f ms, want within %.1f-%.1f ms (+/-1 ms of the budget) — "+
					"the delay OutputLatency reports must be the delay the waveform actually shows",
					shiftMS, groupDelayMinMS, groupDelayMaxMS)
			}
		})
	}
}

// --- benchmark -------------------------------------------------------------

// BenchmarkPCMResampler measures one 20 ms frame through the filter at every
// candidate quality, and reports what that quality costs: alias is the spurious
// image left in the destination band (full scale, lower is better) and delay_ms
// is the filter's group delay. OutputLatency scales with the filter length, so
// the quality choice is also a latency choice — the room bridge wraps both
// directions, and pays delay_ms twice on a rate-crossing call.
//
// The filter runs on the per-participant IO goroutines and the player streams,
// never on the 20 ms mix tick — which is why this is its own benchmark and not
// part of BenchmarkMixTick.
func BenchmarkPCMResampler(b *testing.B) {
	cases := []struct {
		name           string
		src, dst       int
		toneHz, spurHz float64
	}{
		// 6 kHz is above the 4 kHz destination Nyquist; it folds to 2 kHz.
		{"16k_to_8k", 16000, 8000, 6000, 2000},
		// A 3 kHz tone at 8 kHz images at 8000-3000 = 5 kHz once upsampled.
		{"8k_to_16k", 8000, 16000, 3000, 5000},
		// 10 kHz folds to 2 kHz at the 8 kHz destination.
		{"44k_to_8k", 44100, 8000, 10000, 2000},
	}
	for _, c := range cases {
		for _, q := range []int{0, 2, 3, 4, 6, 8, 10} {
			b.Run(fmt.Sprintf("%s/q%d", c.name, q), func(b *testing.B) {
				srcFrame := c.src * 20 / 1000
				frame := tonePCM(srcFrame, 0, c.toneHz, c.src, 0.9)

				// Measure the spur left in the destination band over a
				// settled second of output.
				r := newPCMResampler(c.src, c.dst, q)
				var settled []int16
				for i := range 50 {
					settled = append(settled, decodePCM(r.ResampleBytes(tonePCM(srcFrame, i*srcFrame, c.toneHz, c.src, 0.9)))...)
				}
				alias := toneAmplitude(settled[len(settled)/2:], c.spurHz, c.dst)
				delayMS := float64(r.OutputLatency()) / float64(c.dst) * 1000

				bench := newPCMResampler(c.src, c.dst, q)
				b.ReportAllocs()
				for b.Loop() {
					bench.ResampleBytes(frame)
				}
				// b.Loop resets user metrics on entry, so report after it.
				b.StopTimer()
				b.ReportMetric(delayMS, "delay_ms")
				b.ReportMetric(alias, "alias")
			})
		}
	}
}
