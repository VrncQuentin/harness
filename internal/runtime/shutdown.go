package runtime

import (
	"context"
	"log/slog"
	"time"

	"github.com/VrncQuentin/harness/internal/memory"
)

// defaultDrainTimeout bounds each individual wait in a shutdown attempt when
// the caller does not supply one.
const defaultDrainTimeout = 5 * time.Second

// ShutdownResult reports the outcome of one shutdown attempt.
type ShutdownResult struct {
	// TimedOut reports that at least one bounded drain or wait expired before
	// the corresponding component reported quiescence or termination. Timeout
	// is not termination: components whose termination is unconfirmed keep
	// their ownership and are retried by a later Shutdown.
	TimedOut bool
	// Completed reports that every owned component reached a terminal state
	// and no ownership was retained for a later retry.
	Completed bool
}

// Shutdown runs one pass of the explicit shutdown lifecycle, serialized with
// the apply transaction so a shutdown cannot interleave with a config apply or
// a project edit. The lifecycle is explicit:
//
//  1. stop admissions — the request queue refuses new work;
//  2. cancel the root/task contexts;
//  3. bounded drain — cancel task loops, flush live sessions, wait for the
//     queue and process managers;
//  4. stop API/queue/process components under the timeout ownership protocol;
//  5. release only resources proven idle;
//  6. retain ownership for anything whose termination is unconfirmed.
//
// rootCancel cancels the runtime's root context (process managers, the queue
// worker, and UI request contexts). When rootCancel is nil the caller owns the
// service contexts and no process-manager wait is attempted. drainTimeout
// bounds each individual wait; zero uses defaultDrainTimeout.
//
// A later Shutdown retries the components retained by this one, so a timed-out
// attempt never strands resources whose termination is still unconfirmed.
func (rt *Runtime) Shutdown(rootCancel context.CancelFunc, drainTimeout time.Duration) ShutdownResult {
	rt.applyMu.Lock()
	defer rt.applyMu.Unlock()

	if drainTimeout <= 0 {
		drainTimeout = defaultDrainTimeout
	}

	result := ShutdownResult{Completed: true}

	// 1. Stop admissions before anything else: once shutdown begins, new UI/API
	// chat or task work that reaches the request queue is refused.
	rt.mu.Lock()
	q := rt.reqQueue
	tasks := rt.taskRunner
	rt.mu.Unlock()
	if q != nil {
		q.CloseAdmissions()
	}
	rt.emitShutdownHook("admissions-closed")

	// 2. Cancel the root context so process managers, the queue worker, and UI
	// request contexts observe shutdown before any wait begins.
	if rootCancel != nil {
		rootCancel()
	}
	rt.emitShutdownHook("root-cancelled")

	// 3. Bounded drain. Every wait below is context-aware or has an explicit
	// bound; a wait that expires marks the attempt as timed out rather than
	// hanging shutdown indefinitely. The session flush is run detached and
	// bounded by the drain context because a save can block on saveMu or a
	// hung summarizer; the flush goroutine then keeps running, and the
	// retained session manager and generation let a later Shutdown retry it.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), drainTimeout)
	if tasks != nil {
		if err := tasks.CancelAll(drainCtx); err != nil {
			slog.Warn("runtime shutdown: task loop wait", "err", err)
			result.TimedOut = true
			result.Completed = false
		}
	}
	rt.emitShutdownHook("tasks-cancelled")
	if mgr := rt.SessionManager(); mgr != nil {
		flushDone := make(chan error, 1)
		go func() {
			flushDone <- mgr.FlushAll(drainCtx)
		}()
		select {
		case err := <-flushDone:
			if err != nil {
				slog.Warn("runtime shutdown: session flush", "err", err)
				result.TimedOut = true
				result.Completed = false
			}
		case <-drainCtx.Done():
			slog.Warn("runtime shutdown: session flush wait expired; session ownership retained")
			result.TimedOut = true
			result.Completed = false
		}
	}
	drainCancel()
	rt.emitShutdownHook("sessions-flushed")

	// 4. Stop API servers under the timeout ownership protocol, then the
	// queue and process managers.
	if !rt.stopAPIServers() {
		result.TimedOut = true
		result.Completed = false
	}
	rt.emitShutdownHook("api-stopped")

	if q != nil {
		waitCtx, waitCancel := context.WithTimeout(context.Background(), drainTimeout)
		if !q.Wait(waitCtx) {
			slog.Warn("runtime shutdown: queue drain wait expired; queue ownership retained")
			result.TimedOut = true
			result.Completed = false
		}
		waitCancel()
	}
	rt.emitShutdownHook("queue-wait")

	// Process managers exit only after their context (the root context) is
	// cancelled, so the wait is attempted only when the caller supplied
	// rootCancel and only for managers whose Run loop actually started — a
	// constructed-but-never-launched manager never closes its done channel.
	if rootCancel != nil {
		rt.mu.Lock()
		llamaMgr := rt.llamaMgr
		embedMgr := rt.embedMgr
		rt.mu.Unlock()
		waitCtx, waitCancel := context.WithTimeout(context.Background(), drainTimeout)
		if llamaMgr != nil && llamaMgr.Started() {
			if err := llamaMgr.Wait(waitCtx); err != nil {
				slog.Warn("runtime shutdown: llama manager wait", "err", err)
				result.TimedOut = true
				result.Completed = false
			}
		}
		if embedMgr != nil && embedMgr.Started() {
			if err := embedMgr.Wait(waitCtx); err != nil {
				slog.Warn("runtime shutdown: embedder manager wait", "err", err)
				result.TimedOut = true
				result.Completed = false
			}
		}
		waitCancel()
	}

	// 5+6. Release only resources proven idle; retain ownership for anything
	// whose drain timed out or whose termination is unconfirmed, so a later
	// Shutdown can retry it. A timed-out attempt retains the complete
	// generation — its readers, session manager, and task runner are all
	// generation-bound — because a dependent component is still retained; only
	// a fully completed attempt releases and clears ownership.
	if result.Completed {
		rt.releaseOwnedResources()
	}
	rt.emitShutdownHook("generation-released")
	return result
}

// emitShutdownHook records a shutdown lifecycle transition when a test seam is
// installed. Nil on every production path.
func (rt *Runtime) emitShutdownHook(step string) {
	if rt.shutdownHook != nil {
		rt.shutdownHook(step)
	}
}

// stopAPIServers stops the live API server plus every pending-retired and
// previously-retired server under the timeout ownership protocol. A server
// whose Stop does not confirm termination keeps a retained slot, so the
// runtime never clears the pointer to a still-serving component. It reports
// whether every server confirmed termination. Must be called without rt.mu.
func (rt *Runtime) stopAPIServers() bool {
	allConfirmed := true
	rt.mu.Lock()
	live := rt.apiServer
	rt.mu.Unlock()
	if live != nil {
		if rt.stopAPIServer(live) {
			rt.mu.Lock()
			if rt.apiServer == live {
				rt.apiServer = nil
			}
			rt.mu.Unlock()
		} else {
			allConfirmed = false
		}
	}
	return rt.drainRetiredAPI() && allConfirmed
}

// releaseOwnedResources drops the runtime's ownership of every resource proven
// idle. It is called only after every drain confirmed quiescence and every
// component reported termination, so nothing released here is still in use.
// Caller must hold applyMu.
func (rt *Runtime) releaseOwnedResources() {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	g := rt.gen
	global := rt.globalMem
	active := rt.activeMem
	rt.gen = nil
	rt.globalMem = nil
	rt.activeMem = nil
	rt.agentReg = nil
	rt.assembler = nil
	rt.taskRunner = nil
	rt.started = false
	rt.applied = nil
	rt.setSessionManager(nil)
	rt.reqQueue = nil
	if g != nil {
		g.readers = []memory.Repo{global, active}
		g.release()
	} else {
		closeReaders(global, active)
	}
}
