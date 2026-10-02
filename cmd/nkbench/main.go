// Command nkbench measures the parts of NeelKIT whose cost decides whether the
// product works: how fast the recogniser is relative to real time, how long the
// grammar tier takes to resolve an utterance, and how fast the normaliser runs.
//
// It prints a markdown table meant to be pasted into docs/performance.md, and
// optionally a JSON file for tracking over time.
//
// The measurements are whatever the machine running it produces. Nothing here
// carries a baked-in number, and there is no "expected" column to compare
// against: a benchmark that ships with its own results is a marketing document.
//
//	nkbench asr     -models ./models -corpus ./testdata/speech
//	nkbench route   -n 5000
//	nkbench normalize -n 20000
//	nkbench all     -models ./models -corpus ./testdata/speech -json bench.json
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/asr"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/asr/onnx"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/audio"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/intent/grammar"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/normalize"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	var err error
	switch cmd {
	case "asr":
		err = runASR(args)
	case "route":
		err = runRoute(args)
	case "normalize":
		err = runNormalize(args)
	case "all":
		err = runAll(args)
	case "-h", "--help", "help":
		usage()
		return
	case "version":
		fmt.Printf("%+v\n", version.Get())
		return
	default:
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "nkbench: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `nkbench — measure what NeelKIT costs on this machine

Usage:
  nkbench asr       -models DIR -corpus DIR [-workers N] [-runs N]
  nkbench route     [-n N]
  nkbench normalize [-n N]
  nkbench all       -models DIR -corpus DIR [-json FILE]

The ASR corpus is a directory of 16 kHz mono WAV files. Use your own recordings
of the commands you actually say: a number measured on speech that resembles the
workload is worth more than a number measured on read-aloud audiobooks.
`)
}

/* ── report ─────────────────────────────────────────────────────────────── */

type report struct {
	Host      hostInfo       `json:"host"`
	ASR       *asrReport     `json:"asr,omitempty"`
	Route     *latencyReport `json:"route,omitempty"`
	Normalize *latencyReport `json:"normalize,omitempty"`
	Recorded  time.Time      `json:"recorded_at"`
}

type hostInfo struct {
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	CPUs    int    `json:"cpus"`
	GoVer   string `json:"go"`
	Version string `json:"neelkit"`
}

func host() hostInfo {
	return hostInfo{
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
		CPUs:    runtime.NumCPU(),
		GoVer:   runtime.Version(),
		Version: version.Version,
	}
}

type asrReport struct {
	Model     string  `json:"model"`
	Precision string  `json:"precision"`
	Device    string  `json:"device"`
	Workers   int     `json:"workers"`
	Files     int     `json:"files"`
	AudioSec  float64 `json:"audio_seconds"`
	RTFMean   float64 `json:"rtf_mean"`
	RTFP50    float64 `json:"rtf_p50"`
	RTFP95    float64 `json:"rtf_p95"`
	LatP50MS  float64 `json:"latency_p50_ms"`
	LatP95MS  float64 `json:"latency_p95_ms"`
	ConfMean  float64 `json:"confidence_mean"`

	// Accuracy, when reference transcripts are present. WER is word edits
	// (substitutions + insertions + deletions) over reference words, after
	// lowercasing and stripping punctuation.
	WER         *float64         `json:"wer,omitempty"`
	RefClips    int              `json:"reference_clips,omitempty"`
	Transcripts []clipTranscript `json:"transcripts"`
}

type clipTranscript struct {
	File      string `json:"file"`
	Heard     string `json:"heard"`
	Reference string `json:"reference,omitempty"`
	Errors    int    `json:"word_errors,omitempty"`
	RefWords  int    `json:"reference_words,omitempty"`
}

type latencyReport struct {
	Samples int     `json:"samples"`
	P50US   float64 `json:"p50_us"`
	P95US   float64 `json:"p95_us"`
	P99US   float64 `json:"p99_us"`
	MaxUS   float64 `json:"max_us"`
}

/* ── asr ────────────────────────────────────────────────────────────────── */

func runASR(args []string) error {
	fs := flag.NewFlagSet("asr", flag.ExitOnError)
	models := fs.String("models", "./models", "directory holding the ONNX models")
	corpus := fs.String("corpus", "", "directory of 16 kHz mono WAV files")
	workers := fs.Int("workers", 2, "decoder workers")
	runs := fs.Int("runs", 3, "passes over the corpus; the first is discarded")
	_ = fs.Parse(args)

	if *corpus == "" {
		return fmt.Errorf("-corpus is required\n\nRecord a few WAV files of the commands you actually say and point at them")
	}

	rep, err := benchASR(*models, *corpus, *workers, *runs)
	if err != nil {
		return err
	}
	fmt.Print(renderASR(rep))
	return nil
}

func benchASR(modelsDir, corpusDir string, workers, runs int) (*asrReport, error) {
	wavs, err := filepath.Glob(filepath.Join(corpusDir, "*.wav"))
	if err != nil {
		return nil, err
	}
	if len(wavs) == 0 {
		return nil, fmt.Errorf("no .wav files in %s", corpusDir)
	}
	sort.Strings(wavs)

	engine, err := onnx.New(onnx.Config{
		ModelsDir: modelsDir,
		Workers:   workers,
		Device:    "cpu",
		Warmup:    true,
	})
	if err != nil {
		return nil, fmt.Errorf("load models: %w", err)
	}
	defer func() { _ = engine.Close() }()

	// Load everything up front. Disk I/O is not what is being measured, and on
	// a Windows bind mount it would dominate the result.
	type clip struct {
		name    string
		samples []float32
		ref     string // contents of name.txt beside the WAV, if any
		hasRef  bool
	}
	refs, err := loadReferences(corpusDir)
	if err != nil {
		return nil, err
	}

	clips := make([]clip, 0, len(wavs))
	for _, p := range wavs {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		s, err := audio.DecodeWAV(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(p), err)
		}
		c := clip{name: filepath.Base(p), samples: s}
		if ref, ok := refs[c.name]; ok {
			c.ref, c.hasRef = ref, true
		} else if ref, err := os.ReadFile(strings.TrimSuffix(p, filepath.Ext(p)) + ".txt"); err == nil {
			c.ref, c.hasRef = strings.TrimSpace(string(ref)), true
		}
		clips = append(clips, c)
	}

	var (
		rtfs  []float64
		lats  []float64
		confs []float64
		total float64

		heard = make([]string, len(clips))
	)

	ctx := context.Background()
	for run := 0; run < runs; run++ {
		// The first pass pays for lazily allocated arenas inside ONNX Runtime
		// and a cold page cache. Including it would report a number nobody ever
		// experiences after the first sentence of a session.
		keep := run > 0 || runs == 1
		for i, c := range clips {
			started := time.Now()
			res, err := engine.Transcribe(ctx, asr.Request{
				Samples:  c.samples,
				Priority: asr.PriorityFinal,
			})
			wall := time.Since(started)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", c.name, err)
			}
			heard[i] = res.Text
			if !keep {
				continue
			}
			rtfs = append(rtfs, res.RTF)
			lats = append(lats, float64(wall.Microseconds())/1000)
			confs = append(confs, res.Confidence)
			total += res.Duration.Seconds()
		}
	}

	var (
		transcripts     = make([]clipTranscript, 0, len(clips))
		edits, refWords int
		refClips        int
	)
	for i, c := range clips {
		t := clipTranscript{File: c.name, Heard: heard[i]}
		if c.hasRef {
			ref := words(c.ref)
			t.Reference = c.ref
			t.Errors = editDistance(ref, words(heard[i]))
			t.RefWords = len(ref)
			edits += t.Errors
			refWords += len(ref)
			refClips++
		}
		transcripts = append(transcripts, t)
	}

	info := engine.Info()
	rep := &asrReport{
		Model:     info.Model,
		Precision: info.Precision,
		Device:    info.Device,
		Workers:   info.Workers,
		Files:     len(clips),
		AudioSec:  total,
		RTFMean:   mean(rtfs),
		RTFP50:    pct(rtfs, 50),
		RTFP95:    pct(rtfs, 95),
		LatP50MS:  pct(lats, 50),
		LatP95MS:  pct(lats, 95),
		ConfMean:  mean(confs),

		RefClips:    refClips,
		Transcripts: transcripts,
	}
	if refWords > 0 {
		w := float64(edits) / float64(refWords)
		rep.WER = &w
	}
	return rep, nil
}

// loadReferences reads transcripts.tsv from the corpus directory: one
// "filename<TAB>exact words spoken" per line. A missing file is not an error;
// a clip can also carry its reference in a .txt file beside it.
func loadReferences(dir string) (map[string]string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "transcripts.tsv"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, text, ok := strings.Cut(line, "\t")
		if !ok {
			return nil, fmt.Errorf("transcripts.tsv line %d: want filename<TAB>words", i+1)
		}
		refs[strings.TrimSpace(name)] = strings.TrimSpace(text)
	}
	return refs, nil
}

// words lowercases and strips punctuation, so "List the files." and "list the
// files" are the same sentence. Apostrophes inside words are kept.
func words(s string) []string {
	s = strings.ToLower(s)
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\'' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127:
			return r
		default:
			return ' '
		}
	}, s)
	return strings.Fields(s)
}

// editDistance is the word-level Levenshtein distance between a reference and
// a hypothesis: the substitutions, insertions and deletions WER counts.
func editDistance(ref, hyp []string) int {
	prev := make([]int, len(hyp)+1)
	cur := make([]int, len(hyp)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ref); i++ {
		cur[0] = i
		for j := 1; j <= len(hyp); j++ {
			cost := 1
			if ref[i-1] == hyp[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(hyp)]
}

func renderASR(r *asrReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n### Speech recognition\n\n")
	fmt.Fprintf(&b, "%s, %s, %d decoder worker(s) · %d clips, %.1fs of audio\n\n",
		r.Model, r.Precision, r.Workers, r.Files, r.AudioSec)
	fmt.Fprintf(&b, "| metric | value |\n|---|---|\n")
	fmt.Fprintf(&b, "| real-time factor, mean | %.3f× |\n", r.RTFMean)
	fmt.Fprintf(&b, "| real-time factor, p50 | %.3f× |\n", r.RTFP50)
	fmt.Fprintf(&b, "| real-time factor, p95 | %.3f× |\n", r.RTFP95)
	fmt.Fprintf(&b, "| wall-clock per clip, p50 | %.0f ms |\n", r.LatP50MS)
	fmt.Fprintf(&b, "| wall-clock per clip, p95 | %.0f ms |\n", r.LatP95MS)
	fmt.Fprintf(&b, "| decoder confidence, mean | %.3f |\n", r.ConfMean)
	if r.WER != nil {
		fmt.Fprintf(&b, "| word error rate (%d clips with a reference) | %.1f%% |\n", r.RefClips, *r.WER*100)
	}
	if r.RTFMean >= 1 {
		fmt.Fprintf(&b, "\n> RTF is at or above 1.0: this machine cannot keep up with speech in\n> real time. Live transcription will fall behind.\n")
	}

	// What the recogniser actually heard. When accuracy is the complaint, this
	// is the table that says whether the problem is the audio, the model, or
	// the words being said.
	fmt.Fprintf(&b, "\n| clip | heard | reference | errors |\n|---|---|---|---|\n")
	for _, t := range r.Transcripts {
		ref, errs := "—", "—"
		if t.Reference != "" {
			ref, errs = t.Reference, fmt.Sprintf("%d / %d", t.Errors, t.RefWords)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", t.File, t.Heard, ref, errs)
	}
	return b.String()
}

/* ── routing ────────────────────────────────────────────────────────────── */

// The grammar tier is the architectural claim of the project — that the twenty
// things a developer says every day need no model at all. That claim is only
// worth making if the tier is genuinely cheap, so it is measured.
var routeCorpus = []string{
	"list the files",
	"read the file src/main.go",
	"what branch am I on",
	"create a git branch called feature auth",
	"what changed since my last commit",
	"run the tests",
	"search my project for authentication logic",
	"make a note to refactor the audio ring buffer",
	"show me the last ten commits",
	"write a commit message",
	"open the file internal/audio/mel.go",
	"list the files in internal",
	"what is the git status",
	"add a note buy milk",
	"find every mention of websocket",
}

func runRoute(args []string) error {
	fs := flag.NewFlagSet("route", flag.ExitOnError)
	n := fs.Int("n", 5000, "iterations")
	_ = fs.Parse(args)

	rep := benchRoute(*n)
	fmt.Print(renderLatency("Grammar routing", "one utterance through the compiled matcher", rep))
	return nil
}

func benchRoute(n int) *latencyReport {
	m := grammar.NewMatcher()
	// Warm the regex caches before measuring.
	for _, s := range routeCorpus {
		_, _ = m.Match(s)
	}

	samples := make([]float64, 0, n)
	for i := 0; i < n; i++ {
		text := routeCorpus[i%len(routeCorpus)]
		started := time.Now()
		_, _ = m.Match(text)
		samples = append(samples, float64(time.Since(started).Nanoseconds())/1000)
	}
	return summarise(samples)
}

/* ── normalisation ──────────────────────────────────────────────────────── */

var normCorpus = []string{
	"create a git branch called feature dash auth",
	"set the port to eight thousand and eighty",
	"open the file src slash main dot go",
	"send it to me at neel dot das at example dot com",
	"search for jason parsing in the http package",
	"make a note to refactor the audio ring buffer",
}

func runNormalize(args []string) error {
	fs := flag.NewFlagSet("normalize", flag.ExitOnError)
	n := fs.Int("n", 20000, "iterations")
	_ = fs.Parse(args)

	rep := benchNormalize(*n)
	fmt.Print(renderLatency("Normalisation", "one utterance through the repair pass", rep))
	return nil
}

func benchNormalize(n int) *latencyReport {
	opt := normalize.CommandOptions()
	for _, s := range normCorpus {
		_ = normalize.Text(s, opt)
	}

	samples := make([]float64, 0, n)
	for i := 0; i < n; i++ {
		text := normCorpus[i%len(normCorpus)]
		started := time.Now()
		_ = normalize.Text(text, opt)
		samples = append(samples, float64(time.Since(started).Nanoseconds())/1000)
	}
	return summarise(samples)
}

func renderLatency(title, subtitle string, r *latencyReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n### %s\n\n%s · %d samples\n\n", title, subtitle, r.Samples)
	fmt.Fprintf(&b, "| percentile | latency |\n|---|---|\n")
	fmt.Fprintf(&b, "| p50 | %s |\n", micro(r.P50US))
	fmt.Fprintf(&b, "| p95 | %s |\n", micro(r.P95US))
	fmt.Fprintf(&b, "| p99 | %s |\n", micro(r.P99US))
	fmt.Fprintf(&b, "| max | %s |\n", micro(r.MaxUS))
	return b.String()
}

/* ── all ────────────────────────────────────────────────────────────────── */

func runAll(args []string) error {
	fs := flag.NewFlagSet("all", flag.ExitOnError)
	models := fs.String("models", "./models", "directory holding the ONNX models")
	corpus := fs.String("corpus", "", "directory of 16 kHz mono WAV files")
	workers := fs.Int("workers", 2, "decoder workers")
	runs := fs.Int("runs", 3, "passes over the corpus; the first is discarded")
	out := fs.String("json", "", "also write the raw numbers here")
	_ = fs.Parse(args)

	rep := report{Host: host(), Recorded: time.Now().UTC()}

	fmt.Print(renderHost(rep.Host))

	if *corpus != "" {
		a, err := benchASR(*models, *corpus, *workers, *runs)
		if err != nil {
			// A missing model should not cost the two measurements that need no
			// model at all.
			fmt.Fprintf(os.Stderr, "\nskipping ASR: %v\n", err)
		} else {
			rep.ASR = a
			fmt.Print(renderASR(a))
		}
	} else {
		fmt.Fprint(os.Stderr, "\nskipping ASR: no -corpus given\n")
	}

	rep.Route = benchRoute(5000)
	fmt.Print(renderLatency("Grammar routing", "one utterance through the compiled matcher", rep.Route))

	rep.Normalize = benchNormalize(20000)
	fmt.Print(renderLatency("Normalisation", "one utterance through the repair pass", rep.Normalize))

	if *out != "" {
		raw, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(*out, raw, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "\nwrote %s\n", *out)
	}
	return nil
}

func renderHost(h hostInfo) string {
	return fmt.Sprintf(
		"\n## Measured on\n\n| | |\n|---|---|\n| platform | %s/%s |\n| logical CPUs | %d |\n| go | %s |\n| neelkit | %s |\n| date | %s |\n",
		h.OS, h.Arch, h.CPUs, h.GoVer, h.Version, time.Now().Format("2006-01-02"))
}

/* ── statistics ─────────────────────────────────────────────────────────── */

func summarise(samples []float64) *latencyReport {
	return &latencyReport{
		Samples: len(samples),
		P50US:   pct(samples, 50),
		P95US:   pct(samples, 95),
		P99US:   pct(samples, 99),
		MaxUS:   pct(samples, 100),
	}
}

// pct returns the p-th percentile using nearest-rank, which needs no
// interpolation choice to defend and matches what most latency tooling reports.
func pct(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	idx := int(float64(len(s)-1) * p / 100)
	return s[idx]
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	var sum float64
	for _, x := range v {
		sum += x
	}
	return sum / float64(len(v))
}

func micro(us float64) string {
	if us >= 1000 {
		return fmt.Sprintf("%.2f ms", us/1000)
	}
	return fmt.Sprintf("%.1f µs", us)
}
