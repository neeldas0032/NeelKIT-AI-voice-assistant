// Command neelkitd is the NeelKIT server.
//
// It parses configuration, wires the dependency graph, serves HTTP, and shuts
// down in an order that cannot free a model out from under a request that is
// still using it.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/asr"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/asr/onnx"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/assistant"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/auth"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/config"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/httpapi"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/intent"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/llm"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/llm/ollama"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/observability"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/storage"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/policy"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/runner"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/sandbox"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/vad"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/version"
)

func main() {
	if err := run(); err != nil {
		// slog may not be configured yet if the failure was in config loading.
		fmt.Fprintln(os.Stderr, "neelkitd:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		return err
	}
	observability.SetupLogging(cfg.LogLevel, cfg.LogFormat)
	log := slog.Default()

	v := version.Get()
	log.Info("starting neelkitd", "version", v.Version, "commit", v.Commit, "config", cfg.String())

	if cfg.PublicBind() {
		// This service can run programs on the machine. Exposing it beyond
		// loopback should never be something a user discovers later.
		log.Warn("listening beyond loopback; anyone who can reach this port and "+
			"holds an API key can run commands in the workspace",
			"addr", cfg.HTTPAddr)
	}

	// ---- storage -----------------------------------------------------
	db, err := storage.Open(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	defer db.Close()

	ctx := context.Background()
	userID, err := db.EnsureUser(ctx, "local")
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}

	// ---- authentication ----------------------------------------------
	keys := auth.NewStore()
	if err := setupKey(ctx, db, keys, cfg, userID, log); err != nil {
		return err
	}
	tickets := auth.NewTicketStore(30 * time.Second)

	// ---- speech ------------------------------------------------------
	metrics := observability.NewMetrics()

	var engine asr.Engine
	eng, err := onnx.New(onnx.Config{
		ModelsDir:      cfg.ModelsDir,
		ORTLibrary:     cfg.ORTLibrary,
		Workers:        cfg.ASRWorkers,
		Device:         cfg.ASRDevice,
		GPUDevice:      cfg.ASRGPUDevice,
		ChunkSeconds:   cfg.ChunkSeconds,
		OverlapSeconds: cfg.OverlapSecond,
		MaxAudio:       cfg.MaxAudio,
		Warmup:         cfg.ASRWarmup,
		Logger:         log,
	})
	if err != nil {
		// A missing model is the single most likely first-run problem, so it
		// gets an actionable message rather than a stack trace.
		return fmt.Errorf("speech engine: %w\n\nRun `make models` to download them, "+
			"or set NEELKIT_MODELS_DIR to where they already are", err)
	}
	engine = eng
	defer engine.Close()

	vadPath := filepath.Join(cfg.ModelsDir, "silero_vad.onnx")
	detector, err := vad.NewDetector(vadPath)
	if err != nil {
		// Batch transcription still works without it; live audio does not.
		log.Warn("voice activity model missing: live sessions are disabled",
			"path", vadPath, "error", err)
	} else {
		defer detector.Close()
	}

	// ---- language model ----------------------------------------------
	var provider llm.Provider
	if cfg.LLMProvider == "ollama" {
		p := ollama.New(ollama.Config{
			BaseURL: cfg.LLMBaseURL, Model: cfg.LLMModel, Timeout: cfg.LLMTimeout,
		})
		hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if err := p.Health(hctx); err != nil {
			// Not fatal. The grammar tier is the whole point of the two-tier
			// router: built-in commands keep working with no model at all.
			log.Warn("language model unreachable; built-in commands still work",
				"base_url", cfg.LLMBaseURL, "error", err)
		} else {
			log.Info("language model ready", "provider", "ollama", "model", cfg.LLMModel)
		}
		cancel()
		provider = p
	}

	// ---- tools -------------------------------------------------------
	sb, err := sandbox.New(sandbox.Config{
		Root:            cfg.WorkspaceRoot,
		ReadOnly:        cfg.WorkspaceRO,
		AllowedBinaries: cfg.AllowedBinaries,
		ExecTimeout:     cfg.ToolTimeout,
		Concurrency:     cfg.ToolConcurrency,
	})
	if err != nil {
		return fmt.Errorf("workspace: %w\n\nSet NEELKIT_WORKSPACE_ROOT to a directory that exists", err)
	}
	defer sb.Close()
	log.Info("workspace ready", "root", sb.Root(), "read_only", cfg.WorkspaceRO,
		"tools", tools.Default.Len())

	pol := policy.DefaultSettings()
	pol.ReadOnly = cfg.WorkspaceRO

	toolRunner := runner.New(runner.Config{
		Registry: tools.Default,
		Sandbox:  sb,
		Policy:   pol,
		Audit:    auditSink{db: db, userID: userID},
		Timeout:  cfg.ToolTimeout,
		Observe: func(tool, outcome string, d time.Duration) {
			metrics.ToolDuration.Observe(d.Seconds(), tool, outcome)
		},
	})

	router := intent.New(intent.Config{
		Registry: tools.Default,
		Provider: provider,
		Timeout:  cfg.LLMTimeout,
	})
	if provider != nil {
		// Load the model and cache the prompt prefix in the background, so the
		// first spoken question does not pay for it. Allowed longer than one
		// request because it includes loading the weights from disk.
		go func() {
			wctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			defer cancel()
			start := time.Now()
			if err := router.Warm(wctx); err != nil {
				log.Warn("language model warm-up failed; the first question will be slow",
					"error", err)
				return
			}
			log.Info("language model warmed", "took_ms", time.Since(start).Milliseconds())
		}()
	}

	asst := assistant.New(assistant.Config{
		Router: router, Runner: toolRunner, DB: db,
		Metrics: metrics, UserID: userID,
	})

	// ---- http --------------------------------------------------------
	srv := httpapi.New(httpapi.Deps{
		Config: cfg, Engine: engine, VAD: detector, Assistant: asst,
		Router: router, Registry: tools.Default, DB: db,
		Keys: keys, Tickets: tickets, Metrics: metrics,
		LLM: provider, UserID: userID,
	})

	httpServer := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: srv.Handler(),
		// ReadHeaderTimeout defends against Slowloris without capping a long
		// upload or a long-lived WebSocket. WriteTimeout is deliberately unset:
		// a global write deadline would sever every live session mid-sentence.
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.HTTPAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-quit:
		log.Info("shutting down", "signal", sig.String())
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	}

	// Shutdown order matters and is the reverse of construction: stop accepting
	// connections, let in-flight requests finish, then release the models they
	// might still be using. Closing the engine first would free an ONNX session
	// out from under a decode that is still running.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Warn("some requests did not finish before the deadline", "error", err)
	}
	log.Info("stopped")
	return nil
}

// setupKey loads or creates the API key.
//
// The key is created once, printed once, and its salted hash is stored in the
// database, so it keeps working across restarts. An earlier version generated
// a fresh key on every start and kept it only in memory, which silently
// invalidated the key saved in the browser each time the server restarted:
// every request failed with 401 and the interface showed nothing useful.
//
// -rotate-key revokes the stored key and issues a new one, for when the old
// one is lost or leaked. NEELKIT_API_KEY, when set, is used instead.
func setupKey(ctx context.Context, db *storage.DB, keys *auth.Store, cfg config.Config, userID string, log *slog.Logger) error {
	if cfg.APIKey != "" {
		rec, err := auth.FromToken(userID, "configured", cfg.APIKey)
		if err != nil {
			return fmt.Errorf("NEELKIT_API_KEY is not a valid key: %w", err)
		}
		keys.Add(rec)
		log.Info("using the API key from the environment", "key_id", rec.ID)
		return nil
	}

	token, err := loadOrCreateKey(ctx, db, keys, userID, cfg.RotateKey)
	if err != nil {
		return err
	}
	if token == "" {
		log.Info("using the stored API key; the key printed on first run still works",
			"hint", "lost it? restart with -rotate-key")
		return nil
	}

	// Printed to stderr, not logged: this is the one value that must never end
	// up in a structured log that gets shipped somewhere.
	fmt.Fprintf(os.Stderr, "\n  NeelKIT API key (shown once, store it now):\n\n    %s\n\n"+
		"  It is saved (hashed) and keeps working after restarts.\n"+
		"  Lost it? Restart with -rotate-key to get a new one.\n\n", token)
	return nil
}

// loadOrCreateKey registers the stored keys, creating one if there are none
// (or if rotate is set). It returns the new token when one was created, and ""
// when an existing key was loaded.
func loadOrCreateKey(ctx context.Context, db *storage.DB, keys *auth.Store, userID string, rotate bool) (string, error) {
	if rotate {
		if _, err := db.RevokeAPIKeys(ctx, userID); err != nil {
			return "", fmt.Errorf("revoke api keys: %w", err)
		}
	} else {
		stored, err := db.ActiveAPIKeys(ctx, userID)
		if err != nil {
			return "", fmt.Errorf("load api keys: %w", err)
		}
		for _, k := range stored {
			keys.Add(auth.Key{
				ID: k.ID, UserID: k.UserID, Name: k.Name,
				Hash: k.SecretHash, Salt: k.Salt, CreatedAt: k.CreatedAt,
			})
		}
		if len(stored) > 0 {
			return "", nil
		}
	}

	token, rec, err := auth.Generate(userID, "default")
	if err != nil {
		return "", err
	}
	if err := db.SaveAPIKey(ctx, storage.APIKey{
		ID: rec.ID, UserID: rec.UserID, Name: rec.Name,
		SecretHash: rec.Hash, Salt: rec.Salt, CreatedAt: rec.CreatedAt,
	}); err != nil {
		return "", fmt.Errorf("save api key: %w", err)
	}
	keys.Add(rec)
	return token, nil
}

// auditSink bridges the tool runner to the audit log.
type auditSink struct {
	db     *storage.DB
	userID string
}

func (a auditSink) ToolStarted(ctx context.Context, p runner.Plan) error {
	return a.db.Audit(ctx, storage.AuditEvent{
		UserID: a.userID, RequestID: observability.RequestID(ctx),
		Actor: "assistant", Action: "tool.start", Subject: p.Tool,
		Risk: string(p.Risk), Outcome: "allowed",
	})
}

func (a auditSink) ToolFinished(ctx context.Context, p runner.Plan, _ tools.Result, err error, d time.Duration) {
	outcome := "allowed"
	if err != nil {
		outcome = "failed"
	}
	_ = a.db.Audit(ctx, storage.AuditEvent{
		UserID: a.userID, RequestID: observability.RequestID(ctx),
		Actor: "assistant", Action: "tool.finish", Subject: p.Tool,
		Risk: string(p.Risk), Outcome: outcome,
		DetailJSON: fmt.Sprintf(`{"duration_ms":%d}`, d.Milliseconds()),
	})
}

func (a auditSink) ToolDenied(ctx context.Context, p runner.Plan) {
	_ = a.db.Audit(ctx, storage.AuditEvent{
		UserID: a.userID, RequestID: observability.RequestID(ctx),
		Actor: "assistant", Action: "tool.deny", Subject: p.Tool,
		Risk: string(p.Risk), Outcome: "denied",
		DetailJSON: fmt.Sprintf(`{"reason":%q}`, p.Decision.Reason),
	})
}
