package audio

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Transcoder converts non-WAV audio to 16 kHz mono WAV using an external
// ffmpeg binary.
//
// WAV is parsed in-process so the common case has no external dependency;
// everything else is delegated, because reimplementing MP3, Opus and AAC
// decoders in Go to avoid one binary would be a worse trade by any measure.
type Transcoder struct {
	once    sync.Once
	path    string
	found   bool
	timeout time.Duration
}

// NewTranscoder returns a transcoder with the given per-conversion timeout.
func NewTranscoder(timeout time.Duration) *Transcoder {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &Transcoder{timeout: timeout}
}

// Available reports whether ffmpeg was found. Probing once at first use rather
// than on every request means a missing binary produces one clear log line and
// a clean 415, not a surprise failure buried in a stack trace.
func (t *Transcoder) Available() bool {
	t.once.Do(func() {
		p, err := exec.LookPath("ffmpeg")
		t.path, t.found = p, err == nil
	})
	return t.found
}

// Convert transcodes arbitrary audio into 16 kHz mono PCM WAV bytes.
//
// Safe for concurrent use: each call creates its own temporary files, which
// matters because several transcriptions can be in flight at once and shared
// paths would corrupt each other.
func (t *Transcoder) Convert(ctx context.Context, data []byte) ([]byte, error) {
	if !t.Available() {
		return nil, fmt.Errorf("%w: not a WAV file and ffmpeg is not installed", ErrUnsupportedAudio)
	}

	in, err := os.CreateTemp("", "neelkit-in-*.bin")
	if err != nil {
		return nil, fmt.Errorf("transcode: temp input: %w", err)
	}
	defer os.Remove(in.Name())
	if _, err := in.Write(data); err != nil {
		in.Close()
		return nil, fmt.Errorf("transcode: write input: %w", err)
	}
	if err := in.Close(); err != nil {
		return nil, fmt.Errorf("transcode: close input: %w", err)
	}

	out, err := os.CreateTemp("", "neelkit-out-*.wav")
	if err != nil {
		return nil, fmt.Errorf("transcode: temp output: %w", err)
	}
	outPath := out.Name()
	out.Close()
	defer os.Remove(outPath)

	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, t.path,
		"-nostdin",           // never block waiting on stdin
		"-hide_banner",       // keep stderr useful
		"-loglevel", "error", //
		"-y", // overwrite the temp file we just made
		"-i", in.Name(),
		"-ac", "1", // mono
		"-ar", "16000", // the only rate the model accepts
		"-acodec", "pcm_s16le",
		"-f", "wav", // force the container regardless of the filename
		outPath,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("%w: conversion timed out after %s", ErrUnsupportedAudio, t.timeout)
		}
		// ffmpeg exiting non-zero means the input is corrupt or in a format it
		// cannot decode. Both are the client's problem, so the error is
		// wrapped as unsupported audio and surfaces as a 4xx, not a 500.
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedAudio, firstLine(stderr.String()))
	}

	wav, err := os.ReadFile(outPath)
	if err != nil {
		return nil, fmt.Errorf("transcode: read output: %w", err)
	}
	if len(wav) < 44 {
		return nil, fmt.Errorf("%w: ffmpeg produced no audio", ErrUnsupportedAudio)
	}
	return wav, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return "conversion failed"
	}
	const max = 200
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
