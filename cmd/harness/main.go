// Command harness is the main entry point for the local AI inference harness.
// It follows the startup sequence defined in docs/architecture.md.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/VrncQuentin/harness/internal/config"
	"github.com/VrncQuentin/harness/internal/db"
	"github.com/VrncQuentin/harness/internal/home"
	"github.com/VrncQuentin/harness/internal/logbuf"
	"github.com/VrncQuentin/harness/internal/metrics"
	"github.com/VrncQuentin/harness/internal/project"
	"github.com/VrncQuentin/harness/internal/retrieval"
	harnessruntime "github.com/VrncQuentin/harness/internal/runtime"
	"github.com/VrncQuentin/harness/internal/tray"
	"github.com/VrncQuentin/harness/internal/ui"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "harness: %v\n", err)
		os.Exit(1)
	}
}

// run performs desktop bootstrap and hands mutable service orchestration to
// internal/runtime once the UI is online.
func run() error {
	first, err := tray.AcquireSingleInstance()
	if err != nil {
		return fmt.Errorf("single-instance check: %w", err)
	}
	if !first {
		return nil
	}

	// GUI launches on Windows may have no stdout handle; route package-level
	// stdout writes into stderr so they reach the same logging sink.
	os.Stdout = os.Stderr
	logRing, llamaRing, embedRing := configureLogging()

	defaults := config.Defaults()
	uiPort := defaults.UI.Port
	var harnessHome, dbPath string
	var startupErrors []error
	if resolvedHome, err := home.Default(); err != nil {
		startupErrors = append(startupErrors, err)
	} else {
		harnessHome = resolvedHome
		dbPath = home.DBPath(harnessHome)
		uiPort = db.PeekUIPort(dbPath, defaults.UI.Port)
	}
	uiURL := fmt.Sprintf("http://localhost:%d", uiPort)

	rootCtx, rootCancel := context.WithCancel(context.Background())

	uiServer := ui.NewServer(uiPort)
	uiServer.SetLogRing(logRing)
	uiServer.SetLlamaOutputRing(llamaRing)
	uiServer.SetEmbedOutputRing(embedRing)
	if err := uiServer.Start(rootCtx); err != nil {
		rootCancel()
		return fmt.Errorf("UI server start: %w", err)
	}
	slog.Info("ui server listening", "url", uiURL)

	binDir, err := binaryDir()
	if err != nil {
		uiServer.AddStartupError(fmt.Errorf("cannot determine binary dir: %w", err))
	} else {
		uiServer.SetBinDir(binDir)
	}
	for _, err := range startupErrors {
		uiServer.AddStartupError(err)
	}

	var harnessDB *db.DB
	var cfgStore config.Store
	var metricsStore metrics.Store
	var traceSink *retrieval.NDJSONSink
	if harnessHome != "" {
		if err := home.Ensure(harnessHome); err != nil {
			uiServer.AddStartupError(err)
		} else {
			slog.Info("harness starting", "binDir", binDir, "home", harnessHome)
			traceSink = installTraceSink(uiServer, harnessHome)
			harnessDB, cfgStore, metricsStore = harnessruntime.OpenDB(uiServer, dbPath, func(slug string) (string, error) {
				return home.ProjectRepoPath(harnessHome, slug)
			})
			if harnessDB != nil {
				slog.Info("harness.db opened", "path", dbPath)
			}
		}
	}
	uiServer.SetConfigStore(cfgStore)
	uiServer.SetMetricsStore(metricsStore)
	if harnessDB != nil {
		uiServer.SetProjectStore(harnessDB.Projects())
		harnessruntime.EnsureProjectMemoryRepo(uiServer, harnessDB.Projects(), "global")
	}

	cfg, configured := loadInitialConfig(uiServer, cfgStore)

	events := harnessruntime.NewEventChannel()
	rt := harnessruntime.New(cfg, cfgStore, harnessruntime.LogRings{
		Log:   logRing,
		Llama: llamaRing,
		Embed: embedRing,
	})
	if harnessDB != nil {
		rt.SetProjectStore(harnessDB.Projects())
	}

	if configured {
		if err := config.Validate(&cfg); err != nil {
			uiServer.AddStartupError(err)
		} else if harnessruntime.ValidatePaths(uiServer, &cfg) {
			rt.Start(rootCtx, uiServer, events, metricsStore)
		}
	}

	go harnessruntime.ForwardEvents(rootCtx, events, uiServer, rt.Managers, rt.QueueStats)

	uiServer.SetRetry(func() ui.ApplyResult {
		return rt.ApplyConfig(rootCtx, uiServer, events, metricsStore)
	})
	uiServer.SetProjectEditor(func(input project.UpdateInput, memoryRepoMode string) (project.Project, error) {
		return rt.EditProject(rootCtx, uiServer, events, metricsStore, input, memoryRepoMode)
	})
	uiServer.SetProcRestarts(rt.RestartLlama, rt.RestartEmbedder)
	uiServer.SetQuit(tray.Quit)

	if cfg.UI.OpenOnStart {
		go func() {
			time.Sleep(200 * time.Millisecond)
			tray.OpenBrowser(uiURL)
		}()
	}

	onQuit := func() {
		slog.Info("harness shutting down")
		// One cohesive shutdown lifecycle owned by the runtime: stop
		// admissions, cancel the root/task contexts, bounded drains, stop
		// API/queue/process components, release only resources proven idle.
		result := rt.Shutdown(rootCancel, 10*time.Second)
		// Close the retrieval trace sink only after a completed shutdown. A
		// timed-out shutdown retains the generation (readers, session manager,
		// task runner); a still-running detached session flush can emit trace
		// rows through them, so closing the sink early would drop them. The
		// tray lifecycle then exits the process, and the OS flushes the sink's
		// still-open file on exit.
		closeTraceSinkOnCompleted(traceSink, result.Completed)
		if harnessDB != nil {
			_ = harnessDB.Close()
		}
	}

	tray.Run(uiURL, onQuit)
	return nil
}

// installTraceSink constructs and installs the production retrieval trace
// sink under harnessHome/logs/retrieval. A construction failure is surfaced as
// a startup error and no sink is installed, so a trace-directory problem never
// silently disables retrieval tracing. The returned sink is owned by run() and
// closed during graceful shutdown.
func installTraceSink(uiServer *ui.Server, harnessHome string) *retrieval.NDJSONSink {
	sink, err := retrieval.NewNDJSONSink(filepath.Join(harnessHome, "logs", "retrieval"), nil)
	if err != nil {
		uiServer.AddStartupError(fmt.Errorf("retrieval trace sink: %w", err))
		return nil
	}
	retrieval.SetDefaultTraceSink(sink)
	return sink
}

// closeTraceSinkOnCompleted closes the retrieval trace sink when a shutdown is
// confirmed complete. A timed-out shutdown retains the sink: the runtime keeps
// the generation (its readers, session manager, and task runner) open because a
// still-running detached session flush may emit trace rows through them, and
// closing the sink early would drop those rows. In the tray lifecycle the
// process exits right after this, and the OS flushes the still-open file; the
// retention here is about not dropping in-flight emission during the exit
// window, not about a later in-process retry. Idempotent: closing a nil or
// already-closed sink is a no-op.
func closeTraceSinkOnCompleted(traceSink *retrieval.NDJSONSink, completed bool) {
	if traceSink == nil {
		return
	}
	if !completed {
		slog.Warn("retrieval trace sink retained: shutdown incomplete; the sink stays open so in-flight emission is not dropped before process exit")
		return
	}
	if err := traceSink.Close(); err != nil {
		slog.Error("close retrieval trace sink", "err", err)
	}
	retrieval.SetDefaultTraceSink(nil)
}

func configureLogging() (*logbuf.Ring, *logbuf.Ring, *logbuf.Ring) {
	defaults := config.Defaults()
	logRing := logbuf.New(defaults.Log.RingMaxEntries)
	logSink := tee(os.Stderr, logRing)
	log.SetOutput(logSink)
	slog.SetDefault(slog.New(slog.NewTextHandler(logSink, nil)))

	llamaRing := logbuf.New(defaults.Log.ProcMaxLines)
	embedRing := logbuf.New(defaults.Log.ProcMaxLines)
	return logRing, llamaRing, embedRing
}

func loadInitialConfig(uiServer *ui.Server, cfgStore config.Store) (config.Config, bool) {
	cfg := config.Defaults()
	configured := false
	if cfgStore != nil {
		loaded, wasSaved, err := cfgStore.Load()
		if err != nil {
			slog.Error("config load failed", "err", err)
			uiServer.AddStartupError(fmt.Errorf("config load: %w", err))
		} else {
			cfg = *loaded
			configured = wasSaved
		}
	}
	uiServer.SetFirstRun(!configured)
	if configured {
		slog.Info("config loaded", "model_port", cfg.Model.Port, "embed_port", cfg.Embedder.Port)
	} else {
		slog.Info("first run: waiting for config")
	}
	return cfg, configured
}

func binaryDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("os.Executable: %w", err)
	}
	return filepath.Dir(exe), nil
}

type teeWriter struct {
	writers []io.Writer
}

func tee(ws ...io.Writer) *teeWriter {
	return &teeWriter{writers: ws}
}

func (t *teeWriter) Write(p []byte) (int, error) {
	for _, w := range t.writers {
		_, _ = w.Write(p)
	}
	return len(p), nil
}
