// Package audio converts arbitrary audio bytes into the mono 16 kHz float32
// samples the ASR model expects, and turns those samples into mel-spectrogram
// features.
package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// SampleRate is the only rate the model accepts. Everything is converted to it.
const SampleRate = 16000

// ErrUnsupportedAudio marks input the pipeline cannot decode. The HTTP layer
// maps it to 400 rather than 500: a payload we cannot parse is the client's
// problem, not a server fault. Callers must use errors.Is, not string matching.
var ErrUnsupportedAudio = errors.New("unsupported or malformed audio")

// IsWAV reports whether data begins with a RIFF/WAVE header.
//
// Detection is by content, never by filename: an uploaded file may have no
// extension or a misleading one, and the server only ever sees bytes.
func IsWAV(data []byte) bool {
	return len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WAVE"
}

// DecodeWAV parses a RIFF/WAVE payload into mono float32 samples in [-1, 1]
// resampled to SampleRate. Supported encodings: 8/16/24/32-bit integer PCM and
// 32/64-bit IEEE float.
func DecodeWAV(data []byte) ([]float32, error) {
	if len(data) < 44 {
		return nil, fmt.Errorf("%w: wav shorter than a minimal header", ErrUnsupportedAudio)
	}
	if !IsWAV(data) {
		return nil, fmt.Errorf("%w: missing RIFF/WAVE magic", ErrUnsupportedAudio)
	}

	var (
		format     uint16
		channels   uint16
		rate       uint32
		bits       uint16
		haveFormat bool
	)

	// Walk the chunk list. Chunks may appear in any order and a conforming file
	// can carry LIST/fact/cue chunks between fmt and data, so we cannot assume
	// the canonical 44-byte layout.
	for off := 12; off+8 <= len(data); {
		chunkID := string(data[off : off+4])
		size := int(binary.LittleEndian.Uint32(data[off+4 : off+8]))
		body := off + 8
		if size < 0 || body > len(data) {
			return nil, fmt.Errorf("%w: chunk %q overruns the file", ErrUnsupportedAudio, chunkID)
		}
		end := body + size
		if end > len(data) {
			// Truncated final chunk: salvage what is present rather than failing.
			end = len(data)
		}

		switch chunkID {
		case "fmt ":
			if size < 16 {
				return nil, fmt.Errorf("%w: fmt chunk is %d bytes, need 16", ErrUnsupportedAudio, size)
			}
			format = binary.LittleEndian.Uint16(data[body : body+2])
			channels = binary.LittleEndian.Uint16(data[body+2 : body+4])
			rate = binary.LittleEndian.Uint32(data[body+4 : body+8])
			bits = binary.LittleEndian.Uint16(data[body+14 : body+16])
			// WAVE_FORMAT_EXTENSIBLE stores the real codec in the GUID's first
			// two bytes; without this, every 24-bit file from a DAW is rejected.
			if format == 0xFFFE && size >= 40 {
				format = binary.LittleEndian.Uint16(data[body+24 : body+26])
			}
			haveFormat = true

		case "data":
			if !haveFormat {
				return nil, fmt.Errorf("%w: data chunk precedes fmt chunk", ErrUnsupportedAudio)
			}
			if channels == 0 || rate == 0 {
				return nil, fmt.Errorf("%w: fmt declares %d channels at %d Hz", ErrUnsupportedAudio, channels, rate)
			}
			samples, err := pcmToFloat32(data[body:end], format, channels, bits)
			if err != nil {
				return nil, err
			}
			return Resample(samples, int(rate), SampleRate), nil
		}

		off = body + size
		if size%2 != 0 {
			off++ // RIFF chunks are word-aligned with a pad byte.
		}
	}
	return nil, fmt.Errorf("%w: no data chunk", ErrUnsupportedAudio)
}

func pcmToFloat32(data []byte, format, channels, bits uint16) ([]float32, error) {
	const (
		formatPCM   = 1
		formatFloat = 3
	)
	if format != formatPCM && format != formatFloat {
		return nil, fmt.Errorf("%w: codec %d is not PCM", ErrUnsupportedAudio, format)
	}
	bytesPer := int(bits) / 8
	if bytesPer == 0 {
		return nil, fmt.Errorf("%w: %d bits per sample", ErrUnsupportedAudio, bits)
	}
	frameSize := bytesPer * int(channels)
	n := len(data) / frameSize
	out := make([]float32, n)

	for i := 0; i < n; i++ {
		var sum float64
		for ch := 0; ch < int(channels); ch++ {
			o := i*frameSize + ch*bytesPer
			var v float64
			switch {
			case bits == 8 && format == formatPCM:
				v = float64(data[o])/128.0 - 1.0 // 8-bit WAV is unsigned
			case bits == 16 && format == formatPCM:
				v = float64(int16(binary.LittleEndian.Uint16(data[o:o+2]))) / 32768.0
			case bits == 24 && format == formatPCM:
				u := int32(data[o]) | int32(data[o+1])<<8 | int32(data[o+2])<<16
				if u&0x800000 != 0 {
					u |= ^0xffffff // sign-extend 24 -> 32
				}
				v = float64(u) / 8388608.0
			case bits == 32 && format == formatPCM:
				v = float64(int32(binary.LittleEndian.Uint32(data[o:o+4]))) / 2147483648.0
			case bits == 32 && format == formatFloat:
				v = float64(math.Float32frombits(binary.LittleEndian.Uint32(data[o : o+4])))
			case bits == 64 && format == formatFloat:
				v = math.Float64frombits(binary.LittleEndian.Uint64(data[o : o+8]))
			default:
				return nil, fmt.Errorf("%w: %d-bit codec %d", ErrUnsupportedAudio, bits, format)
			}
			sum += v
		}
		out[i] = float32(sum / float64(channels)) // downmix to mono
	}
	return out, nil
}

// EncodeWAV writes mono float32 samples as a 16-bit PCM WAV at the given rate.
// Used for fixtures, for the optional audio archive, and by tests.
func EncodeWAV(samples []float32, rate int) []byte {
	dataLen := len(samples) * 2
	buf := make([]byte, 44+dataLen)

	copy(buf[0:], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:], uint32(36+dataLen))
	copy(buf[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(buf[16:], 16)
	binary.LittleEndian.PutUint16(buf[20:], 1) // PCM
	binary.LittleEndian.PutUint16(buf[22:], 1) // mono
	binary.LittleEndian.PutUint32(buf[24:], uint32(rate))
	binary.LittleEndian.PutUint32(buf[28:], uint32(rate*2)) // byte rate
	binary.LittleEndian.PutUint16(buf[32:], 2)              // block align
	binary.LittleEndian.PutUint16(buf[34:], 16)             // bits
	copy(buf[36:], "data")
	binary.LittleEndian.PutUint32(buf[40:], uint32(dataLen))

	for i, s := range samples {
		// Clamp before scaling: a sample above 1.0 would wrap to a large
		// negative int16 and produce an audible click instead of clipping.
		if s > 1 {
			s = 1
		} else if s < -1 {
			s = -1
		}
		binary.LittleEndian.PutUint16(buf[44+i*2:], uint16(int16(s*32767)))
	}
	return buf
}

// PCM16ToFloat32 converts little-endian signed 16-bit PCM (the wire format used
// by the realtime WebSocket) into normalized float32 samples.
func PCM16ToFloat32(b []byte, out []float32) []float32 {
	n := len(b) / 2
	if cap(out) < n {
		out = make([]float32, n)
	}
	out = out[:n]
	for i := 0; i < n; i++ {
		out[i] = float32(int16(binary.LittleEndian.Uint16(b[i*2:]))) / 32768.0
	}
	return out
}
