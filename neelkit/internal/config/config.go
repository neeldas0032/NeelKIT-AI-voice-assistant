// Package config loads and validates NeelKIT's configuration.
//
// Every setting has exactly one name, is documented in .env.example, and is
// validated at startup. A server that starts with a nonsensical configuration
// and fails on the first request is worse than one that refuses to start.
package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// EnvPrefix namespaces every environment variable.
const EnvPrefix = "NEELKIT_"

// Config is the complete server configuration.
type Config struct {
	// --- HTTP ---
	HTTPAddr        string
	CORSOrigins     []string
	RequestTimeout  time.Duration
	ShutdownTimeout time.Duration
	MaxUploadBytes  int64

	// --- ASR ---
	ModelsDir     string
	ORTLibrary    string
	ASRDevice     string
	ASRGPUDevice  int
	ASRWorkers    int
	ASRWarmup     bool
	MaxAudio      time.Duration
	ChunkSeconds  int
	OverlapSecond int

	// --- realtime ---
	Partials     bool
	HangoverMS   int
	PartialEvery time.Duration
	MaxUtterance time.Duration

	// --- LLM ---
	LLMProvider string
	LLMBaseURL  string
	LLMModel    string
	LLMTimeout  time.Duration
	LLMAPIKey   string

	// --- tools ---
	WorkspaceRoot    string
	WorkspaceRO      bool
	ToolTimeout      time.Duration
	ToolConcurrency  int
	AllowedBinaries  []string
	AllowedHTTPHosts []string

	// --- storage ---
	DataDir string

	// --- auth ---
	APIKey string
	// RotateKey revokes every stored key and issues a new one, for a key
	// that was lost or leaked.
	RotateKey  bool
	AuthEnable bool

	// --- observability ---
	LogLevel      string
	LogFormat     string
	MetricsPublic bool
}

// Default returns the shipped configuration.
func Default() Config {
	home, _ := os.UserHomeDir()
	return Config{
		HTTPAddr:        "127.0.0.1:8420",
		CORSOrigins:     []string{"http://localhost:3000", "http://127.0.0.1:3000"},
		RequestTimeout:  120 * time.Second,
		ShutdownTimeout: 30 * time.Second,
		MaxUploadBytes:  50 << 20,

		ModelsDir:     "./models",
		ASRDevice:     "cpu",
		ASRWorkers:    defaultWorkers(),
		ASRWarmup:     true,
		MaxAudio:      4 * time.Hour,
		ChunkSeconds:  300,
		OverlapSecond: 15,

		Partials:     true,
		HangoverMS:   600,
		PartialEvery: 400 * time.Millisecond,
		MaxUtterance: 30 * time.Second,

		LLMProvider: "ollama",
		LLMBaseURL:  "http://127.0.0.1:11434",
		LLMModel:    "qwen2.5:7b-instruct",
		LLMTimeout:  60 * time.Second,

		WorkspaceRoot:   filepath.Join(home, "dev"),
		ToolTimeout:     30 * time.Second,
		ToolConcurrency: 2,

		DataDir: "./data",

		AuthEnable:    true,
		LogLevel:      "info",
		LogFormat:     "json",
		MetricsPublic: false,
	}
}

// defaultWorkers leaves headroom for the encoder and the rest of the process.
// Saturating every core with decoder workers makes latency worse, not better,
// because the encoder then competes with them for the same threads.
func defaultWorkers() int {
	n := runtime.NumCPU() / 2
	if n < 1 {
		n = 1
	}
	if n > 4 {
		n = 4
	}
	return n
}

// Load builds a configuration from defaults, environment variables and flags,
// in that order of increasing precedence.
func Load(args []string) (Config, error) {
	c := Default()

	fs := flag.NewFlagSet("neelkitd", flag.ContinueOnError)
	fs.StringVar(&c.HTTPAddr, "addr", c.HTTPAddr, "HTTP listen address")
	fs.StringVar(&c.ModelsDir, "models", c.ModelsDir, "Directory holding the ASR models")
	fs.StringVar(&c.ORTLibrary, "ort-library", c.ORTLibrary, "Path to libonnxruntime")
	fs.StringVar(&c.ASRDevice, "device", c.ASRDevice, "Execution device: cpu or cuda")
	fs.IntVar(&c.ASRWorkers, "workers", c.ASRWorkers, "Concurrent decoder workers")
	fs.StringVar(&c.WorkspaceRoot, "workspace", c.WorkspaceRoot, "Workspace root that tools are confined to")
	fs.BoolVar(&c.WorkspaceRO, "read-only", c.WorkspaceRO, "Disable all write and execute tools")
	fs.BoolVar(&c.RotateKey, "rotate-key", false, "Revoke the stored API key and print a new one")
	fs.StringVar(&c.DataDir, "data", c.DataDir, "Directory for the database")
	fs.StringVar(&c.LLMProvider, "llm", c.LLMProvider, "LLM provider: ollama, none")
	fs.StringVar(&c.LLMModel, "llm-model", c.LLMModel, "LLM model identifier")
	fs.StringVar(&c.LogLevel, "log-level", c.LogLevel, "Log level: debug, info, warn, error")
	fs.StringVar(&c.LogFormat, "log-format", c.LogFormat, "Log format: json or text")

	// Environment is applied before flags are parsed, so a flag always wins.
	applyEnv(&c)
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if err := c.Validate(); err != nil {
		return c, err
	}
	return c, nil
}

func applyEnv(c *Config) {
	str := func(key string, dst *string) {
		if v, ok := os.LookupEnv(EnvPrefix + key); ok && v != "" {
			*dst = v
		}
	}
	num := func(key string, dst *int) {
		if v, ok := os.LookupEnv(EnvPrefix + key); ok && v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*dst = n
			}
		}
	}
	boolean := func(key string, dst *bool) {
		if v, ok := os.LookupEnv(EnvPrefix + key); ok && v != "" {
			if b, err := strconv.ParseBool(v); err == nil {
				*dst = b
			}
		}
	}
	dur := func(key string, dst *time.Duration) {
		if v, ok := os.LookupEnv(EnvPrefix + key); ok && v != "" {
			if d, err := time.ParseDuration(v); err == nil {
				*dst = d
			}
		}
	}
	list := func(key string, dst *[]string) {
		if v, ok := os.LookupEnv(EnvPrefix + key); ok && v != "" {
			var out []string
			for _, p := range strings.Split(v, ",") {
				if p = strings.TrimSpace(p); p != "" {
					out = append(out, p)
				}
			}
			*dst = out
		}
	}

	str("HTTP_ADDR", &c.HTTPAddr)
	list("CORS_ORIGINS", &c.CORSOrigins)
	dur("REQUEST_TIMEOUT", &c.RequestTimeout)
	dur("SHUTDOWN_TIMEOUT", &c.ShutdownTimeout)

	str("MODELS_DIR", &c.ModelsDir)
	str("ORT_LIBRARY", &c.ORTLibrary)
	str("ASR_DEVICE", &c.ASRDevice)
	num("ASR_GPU_DEVICE", &c.ASRGPUDevice)
	num("ASR_WORKERS", &c.ASRWorkers)
	boolean("ASR_WARMUP", &c.ASRWarmup)
	dur("MAX_AUDIO", &c.MaxAudio)
	num("CHUNK_SECONDS", &c.ChunkSeconds)
	num("OVERLAP_SECONDS", &c.OverlapSecond)

	boolean("PARTIALS", &c.Partials)
	num("HANGOVER_MS", &c.HangoverMS)
	dur("PARTIAL_EVERY", &c.PartialEvery)
	dur("MAX_UTTERANCE", &c.MaxUtterance)

	str("LLM_PROVIDER", &c.LLMProvider)
	str("LLM_BASE_URL", &c.LLMBaseURL)
	str("LLM_MODEL", &c.LLMModel)
	dur("LLM_TIMEOUT", &c.LLMTimeout)
	str("LLM_API_KEY", &c.LLMAPIKey)

	str("WORKSPACE_ROOT", &c.WorkspaceRoot)
	boolean("WORKSPACE_READ_ONLY", &c.WorkspaceRO)
	dur("TOOL_TIMEOUT", &c.ToolTimeout)
	num("TOOL_CONCURRENCY", &c.ToolConcurrency)
	list("ALLOWED_BINARIES", &c.AllowedBinaries)
	list("ALLOWED_HTTP_HOSTS", &c.AllowedHTTPHosts)

	str("DATA_DIR", &c.DataDir)
	str("API_KEY", &c.APIKey)
	boolean("AUTH_ENABLE", &c.AuthEnable)

	str("LOG_LEVEL", &c.LogLevel)
	str("LOG_FORMAT", &c.LogFormat)
	boolean("METRICS_PUBLIC", &c.MetricsPublic)
}

// Validate rejects configurations that cannot work, with a message that says
// what to change.
func (c *Config) Validate() error {
	if c.HTTPAddr == "" {
		return fmt.Errorf("config: HTTP_ADDR is empty")
	}
	switch strings.ToLower(c.ASRDevice) {
	case "cpu", "cuda":
	default:
		return fmt.Errorf("config: ASR_DEVICE is %q, want cpu or cuda", c.ASRDevice)
	}
	if c.ASRWorkers < 1 {
		return fmt.Errorf("config: ASR_WORKERS must be at least 1, got %d", c.ASRWorkers)
	}
	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: LOG_LEVEL is %q", c.LogLevel)
	}
	switch strings.ToLower(c.LLMProvider) {
	case "", "none", "ollama":
	default:
		return fmt.Errorf("config: LLM_PROVIDER is %q, want ollama or none", c.LLMProvider)
	}
	if c.WorkspaceRoot == "" {
		return fmt.Errorf("config: WORKSPACE_ROOT is empty; tools need a directory to be confined to")
	}
	// A wildcard origin on an API that holds a bearer token and can run
	// commands is not a configuration, it is a vulnerability.
	for _, o := range c.CORSOrigins {
		if o == "*" && c.AuthEnable {
			return fmt.Errorf("config: CORS_ORIGINS may not be \"*\" while authentication is enabled")
		}
	}
	if c.HangoverMS < 100 || c.HangoverMS > 5000 {
		return fmt.Errorf("config: HANGOVER_MS is %d, want 100-5000", c.HangoverMS)
	}
	if c.ChunkSeconds <= c.OverlapSecond {
		return fmt.Errorf("config: CHUNK_SECONDS (%d) must exceed OVERLAP_SECONDS (%d)", c.ChunkSeconds, c.OverlapSecond)
	}
	return nil
}

// PublicBind reports whether the server is listening on more than loopback.
// The caller logs a warning: binding a service that can execute commands to a
// public interface should never be silent.
func (c *Config) PublicBind() bool {
	host, _, err := splitHostPort(c.HTTPAddr)
	if err != nil {
		return false
	}
	switch host {
	case "", "127.0.0.1", "localhost", "::1", "[::1]":
		return false
	}
	return true
}

func splitHostPort(addr string) (string, string, error) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return "", "", fmt.Errorf("no port in %q", addr)
	}
	return addr[:i], addr[i+1:], nil
}

// String renders the configuration with every secret redacted, so a config
// dump in a log or an error can never leak one.
func (c Config) String() string {
	redact := func(s string) string {
		if s == "" {
			return "(unset)"
		}
		return "***"
	}
	return fmt.Sprintf(
		"addr=%s device=%s workers=%d models=%s workspace=%s read_only=%v data=%s "+
			"llm=%s/%s api_key=%s llm_api_key=%s log=%s/%s",
		c.HTTPAddr, c.ASRDevice, c.ASRWorkers, c.ModelsDir, c.WorkspaceRoot, c.WorkspaceRO,
		c.DataDir, c.LLMProvider, c.LLMModel, redact(c.APIKey), redact(c.LLMAPIKey),
		c.LogLevel, c.LogFormat)
}
