package mixer

import (
	"encoding/binary"
	"io"
)

// NewResampleReader wraps src to produce PCM at dstRate from srcRate input.
// Returns src unchanged when rates match (passthrough).
func NewResampleReader(src io.Reader, srcRate, dstRate int) io.Reader {
	if srcRate == dstRate {
		return src
	}
	return &resampleReader{src: src, srcRate: srcRate, dstRate: dstRate}
}

// NewResampleWriter wraps dst to accept PCM at inputRate and write at outputRate.
// Returns dst unchanged when rates match (passthrough).
func NewResampleWriter(dst io.Writer, inputRate, outputRate int) io.Writer {
	if inputRate == outputRate {
		return dst
	}
	return &resampleWriter{dst: dst, inputRate: inputRate, outputRate: outputRate}
}

// resampleReader converts PCM from one sample rate to another using linear interpolation.
type resampleReader struct {
	src     io.Reader
	srcRate int
	dstRate int
	buf     []byte // leftover output bytes not yet consumed
}

func (r *resampleReader) Read(p []byte) (int, error) {
	// Serve from leftover buffer first
	if len(r.buf) > 0 {
		n := copy(p, r.buf)
		r.buf = r.buf[n:]
		return n, nil
	}

	// Calculate how many source bytes to read based on the rate ratio.
	// We want to produce len(p) output bytes, so we need approximately
	// len(p) * srcRate / dstRate input bytes. Read in multiples of 2 (one sample).
	srcSize := len(p) * r.srcRate / r.dstRate
	if srcSize < 2 {
		srcSize = 2
	}
	srcSize = (srcSize / 2) * 2

	srcBuf := make([]byte, srcSize)
	n, err := r.src.Read(srcBuf)
	if n < 2 {
		if err != nil {
			return 0, err
		}
		return 0, nil
	}
	n = (n / 2) * 2
	srcBuf = srcBuf[:n]

	numSrcSamples := n / 2
	numDstSamples := numSrcSamples * r.dstRate / r.srcRate
	if numDstSamples == 0 {
		numDstSamples = 1
	}

	out := make([]byte, numDstSamples*2)
	ratio := float64(r.srcRate) / float64(r.dstRate)

	for i := 0; i < numDstSamples; i++ {
		srcPos := float64(i) * ratio
		idx := int(srcPos)
		frac := srcPos - float64(idx)

		var s int16
		if idx+1 < numSrcSamples {
			s0 := int32(int16(binary.LittleEndian.Uint16(srcBuf[idx*2:])))
			s1 := int32(int16(binary.LittleEndian.Uint16(srcBuf[(idx+1)*2:])))
			s = int16(s0 + int32(float64(s1-s0)*frac))
		} else if idx < numSrcSamples {
			s = int16(binary.LittleEndian.Uint16(srcBuf[idx*2:]))
		}
		binary.LittleEndian.PutUint16(out[i*2:], uint16(s))
	}

	copied := copy(p, out)
	if copied < len(out) {
		r.buf = out[copied:]
	}
	return copied, err
}

// resampleWriter converts incoming PCM at inputRate to outputRate before writing.
type resampleWriter struct {
	dst        io.Writer
	inputRate  int
	outputRate int
	buf        []byte // accumulate partial samples
}

func (w *resampleWriter) Write(p []byte) (int, error) {
	total := len(p)

	data := p
	if len(w.buf) > 0 {
		data = append(w.buf, p...)
		w.buf = nil
	}

	// Need at least 2 bytes (one sample)
	usable := (len(data) / 2) * 2
	if usable < 2 {
		w.buf = append(w.buf, data...)
		return total, nil
	}

	remainder := data[usable:]
	data = data[:usable]

	numSrcSamples := usable / 2
	numDstSamples := numSrcSamples * w.outputRate / w.inputRate
	if numDstSamples == 0 {
		numDstSamples = 1
	}

	out := make([]byte, numDstSamples*2)
	ratio := float64(w.inputRate) / float64(w.outputRate)

	for i := 0; i < numDstSamples; i++ {
		srcPos := float64(i) * ratio
		idx := int(srcPos)
		frac := srcPos - float64(idx)

		var s int16
		if idx+1 < numSrcSamples {
			s0 := int32(int16(binary.LittleEndian.Uint16(data[idx*2:])))
			s1 := int32(int16(binary.LittleEndian.Uint16(data[(idx+1)*2:])))
			s = int16(s0 + int32(float64(s1-s0)*frac))
		} else if idx < numSrcSamples {
			s = int16(binary.LittleEndian.Uint16(data[idx*2:]))
		}
		binary.LittleEndian.PutUint16(out[i*2:], uint16(s))
	}

	if len(remainder) > 0 {
		w.buf = append(w.buf, remainder...)
	}

	if _, err := w.dst.Write(out); err != nil {
		return 0, err
	}
	return total, nil
}
