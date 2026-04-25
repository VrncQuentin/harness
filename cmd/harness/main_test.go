package main

import (
	"context"
	"fmt"
	"net"
	"testing"

	"github.com/vrnc/harness/internal/config"
	"github.com/vrnc/harness/internal/queue"
	"github.com/vrnc/harness/internal/ui"
)

func TestStartMemoryAndAPI_InvalidRepoDoesNotBindAPI(t *testing.T) {
	port := freeTCPPort(t)
	cfg := config.Defaults()
	cfg.Memory.RepoPath = t.TempDir()
	cfg.API.Enabled = true
	cfg.API.Port = port

	rt := &runtime{
		cfg:      cfg,
		reqQueue: queue.New(1, "", nil),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rt.startMemoryAndAPI(ctx, ui.NewServer(0))

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("API bound despite invalid memory repo: %v", err)
	}
	_ = ln.Close()
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on ephemeral port: %v", err)
	}
	defer func() { _ = ln.Close() }()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener addr is %T, want *net.TCPAddr", ln.Addr())
	}
	return addr.Port
}
