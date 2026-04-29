package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestParsePair(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    Spec
		wantErr bool
	}{
		{"simple", "0.0.0.0:8443=10.96.1.5:8443", Spec{Listen: "0.0.0.0:8443", Upstream: "10.96.1.5:8443"}, false},
		{"ipv6_listen", "[::]:80=10.96.1.5:80", Spec{Listen: "[::]:80", Upstream: "10.96.1.5:80"}, false},
		{"missing_eq", "0.0.0.0:8443", Spec{}, true},
		{"empty_listen", "=10.96.1.5:8443", Spec{}, true},
		{"empty_upstream", "0.0.0.0:8443=", Spec{}, true},
		{"bad_listen", "not-a-host=10.96.1.5:8443", Spec{}, true},
		{"bad_upstream", "0.0.0.0:8443=also-bad", Spec{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePair(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestServe_EndToEnd starts a real upstream echo server, runs Serve in
// a goroutine, dials through the proxy, and verifies bytes flow both
// ways and that graceful shutdown drains in-flight connections.
func TestServe_EndToEnd(t *testing.T) {
	requireTCPListenSupport(t)

	upstream := startEchoServer(t)

	listenAddr := freeAddr(t)
	specs := []Spec{{Listen: listenAddr, Upstream: upstream}}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	logger := slog.New(slog.DiscardHandler)
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, specs, ServeOptions{DrainTimeout: time.Second}, logger) }()

	conn := dialUntilReady(t, listenAddr, time.Second)
	t.Cleanup(func() { _ = conn.Close() })

	const payload = "ping-pong"
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := readAll(t, conn, len(payload))
	if got != payload {
		t.Fatalf("echo mismatch: got %q want %q", got, payload)
	}

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
}

// TestServe_DrainTimeoutExceeded verifies that Serve returns even when
// a stuck client refuses to drain — proving the drain timeout works.
func TestServe_DrainTimeoutExceeded(t *testing.T) {
	requireTCPListenSupport(t)

	stuck := startStuckUpstream(t)
	listenAddr := freeAddr(t)
	specs := []Spec{{Listen: listenAddr, Upstream: stuck}}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	logger := slog.New(slog.DiscardHandler)
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, specs, ServeOptions{DrainTimeout: 50 * time.Millisecond}, logger) }()

	c := dialUntilReady(t, listenAddr, time.Second)
	t.Cleanup(func() { _ = c.Close() })

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return within drain timeout window")
	}
}

func TestServe_BindFailure(t *testing.T) {
	requireTCPListenSupport(t)

	// Take a port, then ask Serve to bind it.
	occupier, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = occupier.Close() })

	specs := []Spec{{Listen: occupier.Addr().String(), Upstream: "127.0.0.1:1"}}
	logger := slog.New(slog.DiscardHandler)

	err = Serve(context.Background(), specs, ServeOptions{DrainTimeout: time.Second}, logger)
	if err == nil {
		t.Fatal("expected bind failure, got nil")
	}
}

// startEchoServer runs a TCP echo on a random port and returns its addr.
func startEchoServer(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	return l.Addr().String()
}

// startStuckUpstream accepts but never reads or writes — exercises the
// drain-timeout path because splice will block indefinitely.
func startStuckUpstream(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	var keep sync.Mutex
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			keep.Lock()
			t.Cleanup(func() { _ = c.Close() })
			keep.Unlock()
		}
	}()
	return l.Addr().String()
}

// freeAddr reserves an ephemeral port, releases it, and returns the
// address. Inherently racy — fine for tests, not production.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// dialUntilReady polls the proxy listen addr until Serve has bound it.
// Polls (no sleep loop) — bounded by the deadline.
func dialUntilReady(t *testing.T, addr string, within time.Duration) net.Conn {
	t.Helper()
	deadline := time.Now().Add(within)
	var lastErr error
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			return c
		}
		lastErr = err
	}
	t.Fatalf("proxy never came up at %s: %v", addr, lastErr)
	return nil
}

func readAll(t *testing.T, r io.Reader, n int) string {
	t.Helper()
	buf := make([]byte, n)
	got, err := io.ReadFull(r, buf)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read: %v", err)
	}
	return string(buf[:got])
}

func requireTCPListenSupport(t *testing.T) {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err == nil {
		_ = l.Close()
		return
	}
	if errors.Is(err, syscall.EPERM) || errors.Is(err, os.ErrPermission) {
		t.Skipf("skipping test: TCP listen is not permitted in this environment: %v", err)
	}
	t.Fatalf("probe listen failed: %v", err)
}
