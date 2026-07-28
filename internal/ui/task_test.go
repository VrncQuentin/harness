package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// blockingTaskRunner is a TaskRunner whose RunTask blocks on release,
// mirroring blockingChatRunner in chat_test.go, so a test can hold
// handleTaskSend's spawned goroutine in flight for as long as it needs to.
type blockingTaskRunner struct {
	called  chan struct{}
	release chan struct{}
}

func (r *blockingTaskRunner) RunTask(_ context.Context, _, _ string, _ []ChatMessage) (string, <-chan TaskEvent, error) {
	close(r.called)
	<-r.release
	ch := make(chan TaskEvent)
	close(ch)
	return "", ch, nil
}

func (r *blockingTaskRunner) CancelTask(string) error                    { return nil }
func (r *blockingTaskRunner) ApplyApproval(string, string, string) error { return nil }

// TestRenderApprovalHidesAlwaysAllowForShell verifies the approval card omits
// the "Always Allow" control for shell_exec. The approval evaluator forces Ask
// for every shell command regardless of a remembered rule, so offering "always"
// would store an ineffective rule and re-prompt the user forever.
func TestRenderApprovalHidesAlwaysAllowForShell(t *testing.T) {
	s := NewServer(0)

	shell := s.renderTaskEvent(TaskEvent{
		Type:       TaskEventApprovalNeeded,
		ToolID:     "shell_exec",
		ApprovalID: "a1",
		Content:    "approve?",
	})
	if strings.Contains(shell, "Always Allow") || strings.Contains(shell, `"decision":"always"`) {
		t.Fatalf("shell_exec approval must not offer Always Allow:\n%s", shell)
	}
	if !strings.Contains(shell, `"decision":"allow"`) || !strings.Contains(shell, `"decision":"reject"`) {
		t.Fatalf("shell_exec approval must still offer Allow and Reject:\n%s", shell)
	}

	fileWrite := s.renderTaskEvent(TaskEvent{
		Type:       TaskEventApprovalNeeded,
		ToolID:     "edit",
		ApprovalID: "a2",
		Content:    "approve?",
	})
	if !strings.Contains(fileWrite, "Always Allow") || !strings.Contains(fileWrite, `"decision":"always"`) {
		t.Fatalf("non-shell approval should still offer Always Allow:\n%s", fileWrite)
	}
}

// handleTaskSend itself returns as soon as it launches streamTaskEvents in a
// goroutine — but that goroutine goes on reading the TaskRunner dependency
// for as long as the task runs, well past the point the HTTP handler has
// returned. This proves the lease taken on admission is handed off to that
// goroutine rather than released when the handler returns: the drain must
// still be blocked while RunTask hasn't returned, and only completes once it
// does. Mirrors TestHandleChatSendHoldsGenerationLeaseUntilStreamingFinishes.
func TestHandleTaskSendHoldsGenerationLeaseUntilStreamingFinishes(t *testing.T) {
	s := NewServer(3000)
	runner := &blockingTaskRunner{called: make(chan struct{}), release: make(chan struct{})}
	setTaskRunnerForTest(s, runner)

	form := url.Values{"message": {"hi"}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/task/send", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.handleTaskSend(rec, req)

	select {
	case <-runner.called:
	case <-time.After(2 * time.Second):
		t.Fatal("runner was not called")
	}

	drainDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		drainDone <- s.DrainGenerationRequests(ctx)
	}()

	select {
	case <-drainDone:
		t.Fatal("DrainGenerationRequests returned before the streaming goroutine finished")
	case <-time.After(100 * time.Millisecond):
		// Still blocked, as it must be while RunTask holds release.
	}

	close(runner.release)

	select {
	case err := <-drainDone:
		if err != nil {
			t.Fatalf("DrainGenerationRequests: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DrainGenerationRequests did not return after the streaming goroutine finished")
	}
}

// Mirrors TestHandleChatSendRefusesAdmissionWhileADrainIsInProgress: a
// request arriving while a drain is already closing admission must be
// refused before it can spawn a goroutine that would itself go untracked.
func TestHandleTaskSendRefusesAdmissionWhileADrainIsInProgress(t *testing.T) {
	s := NewServer(3000)
	setTaskRunnerForTest(s, &recordingTaskRunner{})

	if err := s.genGate.close(context.Background()); err != nil {
		t.Fatalf("genGate.close: %v", err)
	}

	form := url.Values{"message": {"hi"}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/task/send", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.handleTaskSend(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("handleTaskSend during a drain: status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

// handleTaskSend used to read its TaskRunner before calling s.genGate.enter().
// A reload completing in the gap between the two would leave the request
// admitted against the *new*, reopened generation while runner still pointed
// at the old one — a lease that protects one generation guarding a
// dependency read from a different one. This proves the fix by staging a
// generation swap in the exact window between admission and dependency
// capture (via handleTaskSendHooked's afterEnter hook) and confirming the
// task that actually runs uses the runner current at admission time, not one
// read beforehand.
func TestHandleTaskSendCapturesTheRunnerCurrentAtAdmissionNotBeforeIt(t *testing.T) {
	s := NewServer(3000)
	oldRunner := &recordingTaskRunner{ran: make(chan struct{})}
	setTaskRunnerForTest(s, oldRunner)
	newRunner := &recordingTaskRunner{ran: make(chan struct{})}

	form := url.Values{"message": {"hi"}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/task/send", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	s.handleTaskSendHooked(rec, req, func() {
		// Simulate a reload completing in the window between this request's
		// admission and its dependency capture: swap in a new generation's
		// TaskRunner, as SetServiceDeps would during a real reload.
		setTaskRunnerForTest(s, newRunner)
	})

	select {
	case <-newRunner.ran:
	case <-time.After(2 * time.Second):
		t.Fatal("the runner current at admission time never ran")
	}
	select {
	case <-oldRunner.ran:
		t.Fatal("the stale, pre-admission runner ran instead of the one current when the lease was admitted")
	default:
	}
}
