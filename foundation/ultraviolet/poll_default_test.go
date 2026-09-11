//go:build !windows
// +build !windows

package uv

import (
	"os"
	"testing"
	"time"
)

func TestReader(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("expected no error, but got %s", err)
	}
	defer pw.Close()
	defer pr.Close()

	pollReader, err := newPollReader(pr)
	if err != nil {
		t.Fatalf("expected no error, but got %s", err)
	}
	defer pollReader.Close()

	type pollResult struct {
		ready bool
		err   error
	}
	done := make(chan pollResult, 1)
	go func() {
		ready, err := pollReader.Poll(-1)
		done <- pollResult{ready, err}
	}()

	if !pollReader.Cancel() {
		t.Errorf("expected cancellation to be success")
	}

	select {
	case result := <-done:
		if result.ready || result.err != ErrCanceled {
			t.Fatalf("expected canceled poll, got ready=%t, err=%v", result.ready, result.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("expected cancellation to unblock reader")
	}
	p := make([]byte, 5)
	n, err := pollReader.Read(p)
	if n != 0 {
		t.Errorf("expected 0 bytes read but got %d", n)
	}
	if err != ErrCanceled {
		t.Errorf("expected cancel error but got %v", err)
	}

	// Test that read is still possible after cancellation.
	pollReader, err = newPollReader(pr)
	if err != nil {
		t.Fatalf("expected no error, but got %s", err)
	}
	defer pollReader.Close()
	msg := "hello"
	n, err = pw.Write([]byte(msg))
	if n != len(msg) {
		t.Errorf("expected %d bytes written but got %d", len(msg), n)
	}
	if err != nil {
		t.Fatalf("expected no error, but got %s", err)
	}
	n, err = pollReader.Read(p)
	if n != len(msg) {
		t.Errorf("expected %d bytes read but got %d", len(msg), n)
	}
	if err != nil {
		t.Errorf("expected no error, but got %s", err)
	}
	if string(p[:n]) != msg {
		t.Errorf("expected to read %q but got %q", msg, string(p[:n]))
	}
}
