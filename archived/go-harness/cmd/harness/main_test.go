package main

import (
	"bytes"
	"testing"
)

func TestTeeWritesAllSinks(t *testing.T) {
	var left, right bytes.Buffer
	w := tee(&left, &right)

	n, err := w.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if n != len("hello") {
		t.Fatalf("Write returned %d bytes, want %d", n, len("hello"))
	}
	if left.String() != "hello" || right.String() != "hello" {
		t.Fatalf("tee wrote left=%q right=%q, want both hello", left.String(), right.String())
	}
}
