package runtime

import (
	"context"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/vrnc/harness/internal/metrics"
	"github.com/vrnc/harness/internal/proc"
	"github.com/vrnc/harness/internal/queue"
	"github.com/vrnc/harness/internal/ui"
)

// recordMetrics periodically writes process and queue metrics to the store.
func recordMetrics(
	ctx context.Context,
	store metrics.Store,
	llamaMgr, embedMgr *proc.Manager,
	q *queue.Queue,
	getRetentionDays func() int,
) {
	rec := metrics.NewRecorder(store)
	start := time.Now()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	retentionTicker := time.NewTicker(time.Hour)
	defer retentionTicker.Stop()
	applyMetricRetention(store, getRetentionDays)

	for {
		select {
		case <-ctx.Done():
			return
		case <-retentionTicker.C:
			applyMetricRetention(store, getRetentionDays)
		case <-ticker.C:
			_ = rec.Uptime(time.Since(start))
			if q != nil {
				_ = rec.QueueDepth(q.Depth())
			}
			if llamaMgr != nil {
				st := llamaMgr.Status()
				_ = rec.ProcessHealth("llama-server", st.Healthy)
				_ = rec.ProcessRestartCount("llama-server", st.RestartCount)
			}
			if embedMgr != nil {
				st := embedMgr.Status()
				_ = rec.ProcessHealth("embedder", st.Healthy)
				_ = rec.ProcessRestartCount("embedder", st.RestartCount)
			}
			for _, sample := range pollVRAM(ctx) {
				_ = rec.VRAMUsedMB(sample.gpu, sample.usedMB)
			}
		}
	}
}

func applyMetricRetention(store metrics.Store, getRetentionDays func() int) {
	if store == nil || getRetentionDays == nil {
		return
	}
	days := getRetentionDays()
	if days < 1 {
		return
	}
	if err := store.ApplyRetention(days); err != nil {
		slog.Warn("metrics retention", "err", err)
	}
}

type vramSample struct {
	gpu    string
	usedMB float64
}

func pollVRAM(ctx context.Context) []vramSample {
	cmdCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "nvidia-smi", "--query-gpu=index,memory.used", "--format=csv,noheader,nounits")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	samples := make([]vramSample, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) != 2 {
			continue
		}
		gpu := strings.TrimSpace(parts[0])
		used, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if err != nil {
			continue
		}
		samples = append(samples, vramSample{gpu: gpu, usedMB: used})
	}
	return samples
}

// ForwardEvents reads process events, logs them, and updates the UI state.
func ForwardEvents(
	ctx context.Context,
	events <-chan proc.Event,
	uiSrv *ui.Server,
	getMgrs func() (*proc.Manager, *proc.Manager),
	getQueueStats func() (int, int),
) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			logProcEvent(ev)
			l, e := getMgrs()
			pushStatus(l, "llama-server", uiSrv.SetLlamaStatus)
			pushStatus(e, "embedder", uiSrv.SetEmbedderStatus)
			pushQueueDepth(uiSrv, getQueueStats)
		case <-ticker.C:
			l, e := getMgrs()
			pushStatus(l, "llama-server", uiSrv.SetLlamaStatus)
			pushStatus(e, "embedder", uiSrv.SetEmbedderStatus)
			pushQueueDepth(uiSrv, getQueueStats)
		}
	}
}

func logProcEvent(ev proc.Event) {
	attrs := []any{"process", ev.Process, "kind", string(ev.Kind), "msg", ev.Message}
	switch ev.Kind {
	case proc.EventHealthOK:
		slog.Debug("proc event", attrs...)
	case proc.EventHealthFail, proc.EventStop:
		slog.Warn("proc event", attrs...)
	case proc.EventError, proc.EventFailed:
		slog.Error("proc event", attrs...)
	default:
		slog.Info("proc event", attrs...)
	}
}

func pushQueueDepth(uiSrv *ui.Server, getQueueStats func() (int, int)) {
	if uiSrv == nil || getQueueStats == nil {
		return
	}
	depth, capacity := getQueueStats()
	uiSrv.SetQueueDepth(depth, capacity)
}
func pushStatus(mgr *proc.Manager, name string, set func(ui.ProcessStatus)) {
	if mgr == nil {
		return
	}
	st := mgr.Status()
	set(ui.ProcessStatus{
		Name:         name,
		Running:      st.Running,
		Healthy:      st.Healthy,
		RestartCount: st.RestartCount,
		LastError:    st.LastError,
		ExitCode:     st.ExitCode,
		Failed:       st.Failed,
	})
}
