package runtime

import (
	"context"
	"log/slog"
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
) {
	rec := metrics.NewRecorder(store)
	start := time.Now()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
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
		}
	}
}

// ForwardEvents reads process events, logs them, and updates the UI state.
func ForwardEvents(
	ctx context.Context,
	events <-chan proc.Event,
	uiSrv *ui.Server,
	getMgrs func() (*proc.Manager, *proc.Manager),
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
		case <-ticker.C:
			l, e := getMgrs()
			pushStatus(l, "llama-server", uiSrv.SetLlamaStatus)
			pushStatus(e, "embedder", uiSrv.SetEmbedderStatus)
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
