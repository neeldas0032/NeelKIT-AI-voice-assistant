// Derived from achetronic/parakeet (Apache-2.0), Copyright 2026 Alby Hernández:
// the TDT greedy decode loop, the model dimensions and the session layout.
// Reimplemented here with the changes described in ATTRIBUTION.md.
// SPDX-License-Identifier: Apache-2.0

// Package onnx implements the ASR engine on top of ONNX Runtime and the
// Parakeet TDT checkpoint.
//
// The model is split into two graphs. The encoder is a Conformer that turns mel
// features into 1024-dimensional frames at one eighth the input rate. The
// decoder+joint graph is a recurrent transducer that, given one encoder frame
// and the previously emitted token, predicts the next token and how many frames
// to skip before the next prediction. That duration head is what makes TDT
// faster than a classic transducer: it jumps over frames instead of stepping
// through every one.
package onnx

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/asr"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/audio"
	ort "github.com/yalue/onnxruntime_go"
)

// Fixed dimensions of the Parakeet TDT 0.6B graphs. These are properties of the
// checkpoint, not choices: changing one produces a shape mismatch at Run time.
const (
	encoderDim      int64 = 1024
	decoderStateDim int64 = 640
	decoderLayers   int64 = 2
	durationClasses int64 = 5

	// minSamples is 100 ms. Below this there is not enough signal for a single
	// analysis window, so the engine returns empty rather than guessing.
	minSamples = audio.SampleRate / 10

	// maxTokensPerFrame stops a degenerate loop: if the model keeps emitting
	// non-blank tokens with zero duration it would never advance the timestep.
	maxTokensPerFrame = 10
)

// token is one decoded unit tagged with its absolute encoder frame and the
// model's confidence in it.
type token struct {
	id       int
	timestep int64
	prob     float64
}

// Config configures the engine.
type Config struct {
	// ModelsDir holds config.json, vocab.txt and the two ONNX graphs.
	ModelsDir string
	// ORTLibrary is the path to libonnxruntime.so. Empty probes common paths.
	ORTLibrary string
	// Workers is the number of concurrent decoder sessions. Each costs roughly
	// 20 MB plus its share of the shared graph.
	Workers int
	// Device is "cpu" or "cuda".
	Device string
	// GPUDevice is the CUDA device index.
	GPUDevice int
	// ChunkSeconds and OverlapSeconds size the long-audio windows.
	ChunkSeconds   int
	OverlapSeconds int
	// MaxAudio bounds a single request.
	MaxAudio time.Duration
	// Warmup runs one dummy inference at startup so the first real request does
	// not pay lazy graph initialisation.
	Warmup bool
	Logger *slog.Logger
}

// modelConfig is the subset of the checkpoint's config.json that matters here.
type modelConfig struct {
	ModelType         string `json:"model_type"`
	FeaturesSize      int    `json:"features_size"`
	SubsamplingFactor int    `json:"subsampling_factor"`
}

// Engine is an asr.Engine backed by ONNX Runtime.
type Engine struct {
	cfg   Config
	mc    modelConfig
	log   *slog.Logger
	fb    *audio.Filterbank
	extra sync.Pool // *audio.Extractor, one per concurrent request

	vocab     []string
	vocabSize int
	blankIdx  int

	encoder   *ort.DynamicAdvancedSession
	encoderMu sync.Mutex
	pool      *pool

	chunkFrames   int64
	overlapFrames int64
	precision     string

	closeOnce sync.Once
	// ready is read by every request and written by Close, which runs
	// concurrently with in-flight requests by design.
	ready atomic.Bool
}

var ortInit sync.Once
var ortInitErr error

// New loads the models and builds the worker pool.
func New(cfg Config) (*Engine, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Workers < 1 {
		cfg.Workers = 1
	}
	if cfg.ChunkSeconds <= 0 {
		cfg.ChunkSeconds = 300
	}
	if cfg.OverlapSeconds < 0 {
		cfg.OverlapSeconds = 15
	}
	if cfg.MaxAudio <= 0 {
		cfg.MaxAudio = 4 * time.Hour
	}

	e := &Engine{cfg: cfg, log: cfg.Logger, blankIdx: 8192}

	raw, err := os.ReadFile(filepath.Join(cfg.ModelsDir, "config.json"))
	if err != nil {
		return nil, fmt.Errorf("read model config: %w (run `make models`)", err)
	}
	if err := json.Unmarshal(raw, &e.mc); err != nil {
		return nil, fmt.Errorf("parse model config: %w", err)
	}
	if e.mc.FeaturesSize == 0 {
		e.mc.FeaturesSize = audio.NumMels
	}
	if e.mc.SubsamplingFactor == 0 {
		e.mc.SubsamplingFactor = 8
	}
	if e.mc.FeaturesSize != audio.NumMels {
		return nil, fmt.Errorf("model expects %d mel bins, extractor produces %d",
			e.mc.FeaturesSize, audio.NumMels)
	}

	if err := e.loadVocab(filepath.Join(cfg.ModelsDir, "vocab.txt")); err != nil {
		return nil, fmt.Errorf("load vocab: %w", err)
	}

	e.fb = audio.NewFilterbank(audio.NumMels, audio.SampleRate)
	e.extra.New = func() any { return e.fb.NewExtractor() }

	fps := int64(e.fb.FramesPerSecond())
	e.chunkFrames = int64(cfg.ChunkSeconds) * fps
	e.overlapFrames = int64(cfg.OverlapSeconds) * fps
	if err := validateChunking(e.chunkFrames, e.overlapFrames, int64(e.mc.SubsamplingFactor)); err != nil {
		return nil, err
	}

	if err := initORT(cfg.ORTLibrary); err != nil {
		return nil, err
	}

	encPath, decPath, precision, err := resolveModels(cfg.ModelsDir)
	if err != nil {
		return nil, err
	}
	e.precision = precision

	sessOpts, err := buildSessionOptions(cfg.Device, cfg.GPUDevice)
	if err != nil {
		return nil, err
	}
	if sessOpts != nil {
		// ORT copies options into each session at creation, so the object can
		// be released once every session below exists.
		defer sessOpts.Destroy()
	}

	e.encoder, err = ort.NewDynamicAdvancedSession(
		encPath,
		[]string{"audio_signal", "length"},
		[]string{"outputs", "encoded_lengths"},
		sessOpts,
	)
	if err != nil {
		return nil, fmt.Errorf("create encoder session: %w", err)
	}

	workers := make([]*decoderWorker, 0, cfg.Workers)
	for i := 0; i < cfg.Workers; i++ {
		w, err := newDecoderWorker(decPath, e.vocabSize, sessOpts)
		if err != nil {
			for _, prev := range workers {
				prev.destroy()
			}
			e.encoder.Destroy()
			return nil, fmt.Errorf("create decoder worker %d: %w", i, err)
		}
		workers = append(workers, w)
	}
	e.pool = newPool(workers)

	// Ready before warm-up, not after: warm-up goes through the ordinary
	// Transcribe path, which refuses to run on an engine that is not ready.
	// Setting the flag afterwards made every warm-up fail with "engine closed"
	// and silently gave up the benefit it exists for.
	e.ready.Store(true)

	if cfg.Warmup {
		if err := e.warmup(); err != nil {
			e.log.Warn("engine warm-up failed; the first request will be slower", "error", err)
		}
	}

	e.log.Info("asr engine ready",
		"model", e.mc.ModelType, "precision", precision, "device", cfg.Device,
		"workers", cfg.Workers, "vocab", e.vocabSize, "subsampling", e.mc.SubsamplingFactor)
	return e, nil
}

func initORT(lib string) error {
	ortInit.Do(func() {
		if lib == "" {
			for _, p := range []string{
				"/opt/onnxruntime/lib/libonnxruntime.so",
				"/usr/lib/libonnxruntime.so",
				"/usr/lib/x86_64-linux-gnu/libonnxruntime.so",
				"/usr/local/lib/libonnxruntime.so",
			} {
				if _, err := os.Stat(p); err == nil {
					lib = p
					break
				}
			}
		}
		if lib == "" {
			ortInitErr = fmt.Errorf("ONNX Runtime not found: set NEELKIT_ORT_LIBRARY")
			return
		}
		ort.SetSharedLibraryPath(lib)
		ortInitErr = ort.InitializeEnvironment()
	})
	return ortInitErr
}

// resolveModels prefers the int8 graphs and falls back to fp32, reporting which
// was chosen so /v1/models can tell the truth about what is loaded.
func resolveModels(dir string) (enc, dec, precision string, err error) {
	type pair struct{ enc, dec, name string }
	for _, p := range []pair{
		{"encoder-model.int8.onnx", "decoder_joint-model.int8.onnx", "int8"},
		{"encoder-model.onnx", "decoder_joint-model.onnx", "fp32"},
	} {
		ep, dp := filepath.Join(dir, p.enc), filepath.Join(dir, p.dec)
		_, e1 := os.Stat(ep)
		_, e2 := os.Stat(dp)
		if e1 == nil && e2 == nil {
			return ep, dp, p.name, nil
		}
	}
	return "", "", "", fmt.Errorf("no encoder/decoder models in %s (run `make models`)", dir)
}

func buildSessionOptions(device string, gpuDevice int) (*ort.SessionOptions, error) {
	switch strings.ToLower(strings.TrimSpace(device)) {
	case "", "cpu":
		// nil means ORT defaults, which is exactly the CPU path.
		return nil, nil
	case "cuda":
		opts, err := ort.NewSessionOptions()
		if err != nil {
			return nil, fmt.Errorf("session options: %w", err)
		}
		cuda, err := ort.NewCUDAProviderOptions()
		if err != nil {
			opts.Destroy()
			return nil, fmt.Errorf("cuda provider options: %w", err)
		}
		defer cuda.Destroy()
		if err := cuda.Update(map[string]string{
			"device_id": strconv.Itoa(gpuDevice),
			// The default EXHAUSTIVE algorithm search benchmarks every cuDNN
			// convolution on first run and can reserve several GB for a single
			// Conv, which exhausts VRAM on longer audio.
			"cudnn_conv_algo_search": "HEURISTIC",
			"arena_extend_strategy":  "kSameAsRequested",
		}); err != nil {
			opts.Destroy()
			return nil, fmt.Errorf("configure cuda (device %d): %w", gpuDevice, err)
		}
		if err := opts.AppendExecutionProviderCUDA(cuda); err != nil {
			opts.Destroy()
			// Deliberately fatal. A GPU box silently running CPU inference is
			// the worst failure mode available: everything works, ten times
			// slower, and nothing says why.
			return nil, fmt.Errorf("enable cuda execution provider: %w", err)
		}
		return opts, nil
	default:
		return nil, fmt.Errorf("unsupported device %q (want cpu or cuda)", device)
	}
}

func validateChunking(chunk, overlap, subsampling int64) error {
	if chunk <= 0 {
		return fmt.Errorf("chunk size must be positive, got %d frames", chunk)
	}
	if overlap < 0 || overlap >= chunk {
		return fmt.Errorf("chunk overlap (%d) must be in [0, %d)", overlap, chunk)
	}
	if max := (maxEncoderFrames - safetyMarginFrames) * subsampling; chunk > max {
		return fmt.Errorf("chunk size %d frames exceeds the model-safe maximum %d", chunk, max)
	}
	return nil
}

func (e *Engine) loadVocab(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	entries := map[int]string{}
	maxID := -1
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		// Each line is "<piece> <id>"; the piece itself may contain spaces, so
		// split from the right.
		sep := strings.LastIndexByte(line, ' ')
		if sep < 0 {
			continue
		}
		id, err := strconv.Atoi(line[sep+1:])
		if err != nil {
			continue
		}
		// U+2581 is SentencePiece's word-boundary mark; translating it once
		// here means the decode loop never has to think about it.
		piece := strings.ReplaceAll(line[:sep], "▁", " ")
		entries[id] = piece
		if id > maxID {
			maxID = id
		}
		if piece == "<blk>" {
			e.blankIdx = id
		}
	}
	if maxID < 0 {
		return fmt.Errorf("vocab file %s is empty", path)
	}
	e.vocab = make([]string, maxID+1)
	for id, piece := range entries {
		e.vocab[id] = piece
	}
	e.vocabSize = len(e.vocab)
	return nil
}

// Info implements asr.Engine.
func (e *Engine) Info() asr.Info {
	return asr.Info{
		Model:     "parakeet-tdt-0.6b-v3",
		Precision: e.precision,
		Device:    e.cfg.Device,
		Workers:   e.pool.size(),
		Ready:     e.ready.Load(),
	}
}

// Close implements asr.Engine. It waits for in-flight work before destroying
// anything, so a shutdown during a request cannot free a session out from under
// it.
func (e *Engine) Close() error {
	e.closeOnce.Do(func() {
		e.ready.Store(false)
		if e.pool != nil {
			e.pool.close()
		}
		e.encoderMu.Lock()
		if e.encoder != nil {
			e.encoder.Destroy()
			e.encoder = nil
		}
		e.encoderMu.Unlock()
	})
	return nil
}

func (e *Engine) warmup() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	silence := make([]float32, audio.SampleRate) // one second
	_, err := e.Transcribe(ctx, asr.Request{Samples: silence, Priority: asr.PriorityBatch})
	return err
}

// Transcribe implements asr.Engine.
func (e *Engine) Transcribe(ctx context.Context, req asr.Request) (asr.Result, error) {
	return e.transcribe(ctx, req, nil)
}

// TranscribeStream implements asr.Engine.
func (e *Engine) TranscribeStream(ctx context.Context, req asr.Request, emit func(string)) (asr.Result, error) {
	return e.transcribe(ctx, req, emit)
}

func (e *Engine) transcribe(ctx context.Context, req asr.Request, emit func(string)) (asr.Result, error) {
	start := time.Now()
	if err := ctx.Err(); err != nil {
		return asr.Result{}, err
	}
	if !e.ready.Load() {
		return asr.Result{}, asr.ErrClosed
	}

	duration := time.Duration(len(req.Samples)) * time.Second / audio.SampleRate
	if duration > e.cfg.MaxAudio {
		return asr.Result{}, fmt.Errorf("%w: %s exceeds the %s limit",
			asr.ErrAudioTooLong, duration.Round(time.Second), e.cfg.MaxAudio)
	}
	if len(req.Samples) < minSamples {
		// Not an error: a client that uploads a 50 ms clip gets an empty
		// transcript, which is the truthful answer.
		return asr.Result{Duration: duration}, nil
	}

	ex := e.extra.Get().(*audio.Extractor)
	features := ex.Extract(req.Samples)
	e.extra.Put(ex)
	if len(features) == 0 {
		return asr.Result{Duration: duration}, nil
	}
	// Every request carries a complete slice of audio — an uploaded file, an
	// endpointed utterance, or the trailing window of a preview — so the
	// per-feature statistics the model was trained with are always
	// available. An earlier version used a running (exponentially decaying)
	// normaliser for live audio. Its statistics were dominated by the last
	// second of the slice, which for a finished utterance is mostly the
	// trailing silence, so the speech itself was scaled against silence and
	// the encoder saw input unlike anything it was trained on.
	audio.NormalizeUtterance(features)

	sub := int64(e.mc.SubsamplingFactor)
	windows := planWindows(int64(len(features)), e.chunkFrames, e.overlapFrames, quietestFrame(features))

	var (
		all      []token
		prevTail []token
		queued   time.Duration
	)
	for i, w := range windows {
		emitStart := melToEncoderFrame(w.emitStart-w.start, sub)
		emitEnd := melToEncoderFrame(w.emitEnd-w.start, sub)
		offset := melToEncoderFrame(w.start, sub)

		hold := 0
		var resolve func([]token) []token
		if i > 0 {
			hold = seamTokens
			tail := prevTail
			resolve = func(head []token) []token { return dedupSeam(tail, head) }
		}

		toks, wait, err := e.decodeWindow(ctx, req, features[w.start:w.end], emitStart, emitEnd, offset, hold, resolve, emit)
		queued += wait
		if err != nil {
			return asr.Result{}, err
		}
		all = append(all, toks...)
		prevTail = toks
	}

	res := e.assemble(all, req.WantWords)
	res.Duration = duration
	res.Inference = time.Since(start) - queued
	res.QueueWait = queued
	if duration > 0 {
		res.RTF = res.Inference.Seconds() / duration.Seconds()
	}
	return res, nil
}

// decodeWindow runs the encoder over one window and greedily decodes it.
func (e *Engine) decodeWindow(
	ctx context.Context,
	req asr.Request,
	features [][]float32,
	emitStart, emitEnd, frameOffset int64,
	hold int,
	resolve func([]token) []token,
	emit func(string),
) ([]token, time.Duration, error) {
	numFrames := int64(len(features))
	numFeat := int64(e.mc.FeaturesSize)

	// The encoder wants [batch, features, time]; the extractor produces
	// [time][features]. This transpose is the single largest allocation in the
	// request, so it is done once into a flat slice rather than per frame.
	flat := make([]float32, numFeat*numFrames)
	for t := int64(0); t < numFrames; t++ {
		row := features[t]
		for m := int64(0); m < numFeat && m < int64(len(row)); m++ {
			flat[m*numFrames+t] = row[m]
		}
	}

	in, err := ort.NewTensor(ort.NewShape(1, numFeat, numFrames), flat)
	if err != nil {
		return nil, 0, fmt.Errorf("encoder input tensor: %w", err)
	}
	defer in.Destroy()

	lengths, err := ort.NewTensor(ort.NewShape(1), []int64{numFrames})
	if err != nil {
		return nil, 0, fmt.Errorf("encoder length tensor: %w", err)
	}
	defer lengths.Destroy()

	encodedLen := (numFrames-1)/int64(e.mc.SubsamplingFactor) + 1
	out, err := ort.NewEmptyTensor[float32](ort.NewShape(1, encoderDim, encodedLen))
	if err != nil {
		return nil, 0, fmt.Errorf("encoder output tensor: %w", err)
	}
	defer out.Destroy()

	outLen, err := ort.NewEmptyTensor[int64](ort.NewShape(1))
	if err != nil {
		return nil, 0, fmt.Errorf("encoder output-length tensor: %w", err)
	}
	defer outLen.Destroy()

	// ORT Run is thread-safe on a shared session, but concurrent encoder runs
	// on CPU contend for the same thread pool and make latency unpredictable.
	// Serialising the encoder and parallelising the decoder matches where the
	// time actually goes.
	e.encoderMu.Lock()
	if e.encoder == nil {
		e.encoderMu.Unlock()
		return nil, 0, asr.ErrClosed
	}
	err = e.encoder.Run([]ort.Value{in, lengths}, []ort.Value{out, outLen})
	e.encoderMu.Unlock()
	if err != nil {
		return nil, 0, fmt.Errorf("encoder run: %w", err)
	}

	return e.tdtDecode(ctx, req, out.GetData(), outLen.GetData()[0], emitStart, emitEnd, frameOffset, hold, resolve, emit)
}

// tdtDecode greedily decodes one window's encoder output.
//
// The whole window is decoded so the recurrent state and previous-token
// feedback stay coherent, but only tokens landing in [emitStart, emitEnd) are
// kept: the rest belong to a neighbouring window's region and would duplicate
// speech.
func (e *Engine) tdtDecode(
	ctx context.Context,
	req asr.Request,
	encoderOut []float32,
	encodedLen int64,
	emitStart, emitEnd, frameOffset int64,
	hold int,
	resolve func([]token) []token,
	emit func(string),
) ([]token, time.Duration, error) {
	waitStart := time.Now()
	var (
		w  *decoderWorker
		ok bool
	)
	if req.Priority == asr.PriorityPartial {
		// A preview must never wait: by the time a queued partial is decoded
		// the user has already said more, and it would be stale on arrival.
		if w, ok = e.pool.tryAcquire(); !ok {
			return nil, 0, asr.ErrOverloaded
		}
	} else {
		var err error
		if w, err = e.pool.acquire(ctx); err != nil {
			return nil, time.Since(waitStart), err
		}
	}
	queueWait := time.Since(waitStart)
	defer e.pool.release(w)

	w.resetState()

	var (
		result   []token
		head     []token
		resolved = hold <= 0
		timestep int64
		emitted  int
		prev     = e.blankIdx
	)

	encData := w.encOut.GetData()
	targets := w.targets.GetData()
	s1in, s2in := w.state1In.GetData(), w.state2In.GetData()
	s1out, s2out := w.state1Out.GetData(), w.state2Out.GetData()
	logits := w.output.GetData()

	send := func(t token) {
		result = append(result, t)
		if emit != nil {
			if txt := e.tokenText(t.id); txt != "" {
				emit(txt)
			}
		}
	}
	flushHead := func() {
		survivors := head
		if resolve != nil {
			survivors = resolve(head)
		}
		for _, t := range survivors {
			send(t)
		}
		head = nil
		resolved = true
	}

	for timestep < encodedLen {
		// Copy this encoder frame into the reusable tensor. The encoder output
		// is laid out [dim][time], so a frame is a strided gather.
		for d := int64(0); d < encoderDim; d++ {
			if idx := d*encodedLen + timestep; idx < int64(len(encoderOut)) {
				encData[d] = encoderOut[idx]
			} else {
				encData[d] = 0
			}
		}
		targets[0] = int32(prev)

		if err := w.session.Run(); err != nil {
			return nil, queueWait, fmt.Errorf("decoder run: %w", err)
		}

		vocabLogits := logits[:e.vocabSize]
		durationLogits := logits[e.vocabSize:]

		id, prob := argmaxSoftmax(vocabLogits)
		step := argmax(durationLogits)

		if id != e.blankIdx {
			// Carry the recurrent state forward only on a real emission; a
			// blank leaves the decoder's history untouched.
			copy(s1in, s1out)
			copy(s2in, s2out)
			prev = id
			emitted++

			if timestep >= emitStart && timestep < emitEnd {
				t := token{id: id, timestep: frameOffset + timestep, prob: prob}
				if resolved {
					send(t)
				} else {
					head = append(head, t)
					if len(head) >= hold {
						flushHead()
					}
				}
			}
		}

		select {
		case <-ctx.Done():
			if !resolved {
				flushHead()
			}
			return result, queueWait, ctx.Err()
		default:
		}

		switch {
		case step > 0:
			timestep += int64(step)
			emitted = 0
		case id == e.blankIdx || emitted >= maxTokensPerFrame:
			timestep++
			emitted = 0
		}
	}

	if !resolved {
		flushHead()
	}
	return result, queueWait, nil
}

// tokenText returns the printable text of a token, or "" for special markers.
func (e *Engine) tokenText(id int) string {
	if id < 0 || id >= len(e.vocab) {
		return ""
	}
	t := e.vocab[id]
	if strings.HasPrefix(t, "<") && strings.HasSuffix(t, ">") {
		return ""
	}
	return t
}

// assemble turns decoded tokens into text, word spans and an overall
// confidence.
func (e *Engine) assemble(tokens []token, wantWords bool) asr.Result {
	var sb strings.Builder
	var words []asr.Word
	var logSum float64
	var counted int

	// Seconds of audio represented by one encoder frame: subsampling x hop.
	frameSec := float64(e.mc.SubsamplingFactor) * float64(audio.HopLength) / float64(audio.SampleRate)

	var cur strings.Builder
	var curStart, curEnd int64
	var curLogSum float64
	var curCount int

	flushWord := func() {
		if cur.Len() == 0 {
			return
		}
		text := strings.TrimSpace(cur.String())
		cur.Reset()
		if text == "" {
			return
		}
		conf := 1.0
		if curCount > 0 {
			conf = math.Exp(curLogSum / float64(curCount))
		}
		words = append(words, asr.Word{
			Word:       text,
			StartMS:    int(float64(curStart) * frameSec * 1000),
			EndMS:      int(float64(curEnd+1) * frameSec * 1000),
			Start:      float64(curStart) * frameSec,
			End:        float64(curEnd+1) * frameSec,
			Confidence: conf,
		})
		curLogSum, curCount = 0, 0
	}

	for _, t := range tokens {
		txt := e.tokenText(t.id)
		if txt == "" {
			continue
		}
		sb.WriteString(txt)

		// Accumulate confidence in log space: a 200-token utterance would
		// underflow float64 long before the product finished.
		if t.prob > 0 {
			logSum += math.Log(t.prob)
			counted++
			curLogSum += math.Log(t.prob)
			curCount++
		}

		if wantWords {
			// A leading space is SentencePiece's word boundary, translated at
			// vocab load time.
			if strings.HasPrefix(txt, " ") {
				flushWord()
				curStart = t.timestep
			}
			if cur.Len() == 0 {
				curStart = t.timestep
			}
			cur.WriteString(txt)
			curEnd = t.timestep
		}
	}
	if wantWords {
		flushWord()
	}

	conf := 0.0
	if counted > 0 {
		conf = math.Exp(logSum / float64(counted))
	}
	return asr.Result{
		Text:       collapseSpaces(sb.String()),
		Words:      words,
		Confidence: conf,
	}
}

func collapseSpaces(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

func argmax(v []float32) int {
	best := 0
	for i := 1; i < len(v); i++ {
		if v[i] > v[best] {
			best = i
		}
	}
	return best
}

// argmaxSoftmax returns the highest-scoring index and its softmax probability.
//
// The maximum is subtracted before exponentiating: raw transducer logits reach
// well past 80, and exp of that overflows float64 to +Inf, turning every
// confidence into NaN.
func argmaxSoftmax(v []float32) (int, float64) {
	if len(v) == 0 {
		return 0, 0
	}
	best := argmax(v)
	max := float64(v[best])
	var sum float64
	for _, x := range v {
		sum += math.Exp(float64(x) - max)
	}
	if sum == 0 {
		return best, 0
	}
	return best, 1 / sum
}
