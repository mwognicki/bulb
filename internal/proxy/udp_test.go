package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestServe_UDP_EndToEnd starts a real UDP echo upstream, runs Serve
// with a UDP spec, sends a packet via the proxy, and verifies the echo
// reply lands back at the same client socket.
func TestServe_UDP_EndToEnd(t *testing.T) {
	requireUDPListenSupport(t)

	upstream := startUDPEchoServer(t)
	listenAddr := freeUDPAddr(t)
	specs := []Spec{{Listen: listenAddr, Upstream: upstream, Protocol: ProtocolUDP}}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	logger := slog.New(slog.DiscardHandler)
	served := make(chan error, 1)
	go func() {
		served <- Serve(ctx, specs, ServeOptions{DrainTimeout: time.Second, UDPIdleTimeout: time.Second}, logger)
	}()

	client := dialUDP(t, listenAddr)
	defer client.Close()

	const payload = "ping-pong-udp"
	writeUDPUntilReady(t, client, []byte(payload), time.Second)
	got := readUDPWithDeadline(t, client, len(payload), time.Second)
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

// TestServe_UDP_DistinctClientsGetDistinctSessions verifies that two
// clients sharing the same proxy frontend each get their own upstream
// session — the second client's reply must not leak to the first.
func TestServe_UDP_DistinctClientsGetDistinctSessions(t *testing.T) {
	requireUDPListenSupport(t)

	upstream := startUDPEchoServer(t)
	listenAddr := freeUDPAddr(t)
	specs := []Spec{{Listen: listenAddr, Upstream: upstream, Protocol: ProtocolUDP}}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	logger := slog.New(slog.DiscardHandler)
	served := make(chan error, 1)
	go func() {
		served <- Serve(ctx, specs, ServeOptions{DrainTimeout: time.Second, UDPIdleTimeout: time.Second}, logger)
	}()

	c1 := dialUDP(t, listenAddr)
	defer c1.Close()
	c2 := dialUDP(t, listenAddr)
	defer c2.Close()

	writeUDPUntilReady(t, c1, []byte("alpha"), time.Second)
	if got := readUDPWithDeadline(t, c1, 5, time.Second); got != "alpha" {
		t.Fatalf("c1 got %q", got)
	}
	if _, err := c2.Write([]byte("bravo")); err != nil {
		t.Fatalf("c2 write: %v", err)
	}
	if got := readUDPWithDeadline(t, c2, 5, time.Second); got != "bravo" {
		t.Fatalf("c2 got %q", got)
	}

	cancel()
	<-served
}

// TestServe_UDP_IdleTimeoutClosesSession verifies the per-session
// idle reaper actually fires by checking that the upstream's view of
// the session is gone after the timeout. We do this indirectly: track
// how many distinct upstream source ports the echo server has seen.
func TestServe_UDP_IdleTimeoutClosesSession(t *testing.T) {
	requireUDPListenSupport(t)

	upstream, sources := startUDPEchoServerWithSourceTracking(t)
	listenAddr := freeUDPAddr(t)
	specs := []Spec{{Listen: listenAddr, Upstream: upstream, Protocol: ProtocolUDP}}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	logger := slog.New(slog.DiscardHandler)
	served := make(chan error, 1)
	go func() {
		served <- Serve(ctx, specs, ServeOptions{DrainTimeout: time.Second, UDPIdleTimeout: 200 * time.Millisecond}, logger)
	}()

	client := dialUDP(t, listenAddr)
	defer client.Close()

	// Round 1.
	writeUDPUntilReady(t, client, []byte("first"), time.Second)
	_ = readUDPWithDeadline(t, client, 5, time.Second)

	// Wait past idle timeout + reaper interval (idle/2).
	time.Sleep(500 * time.Millisecond)

	// Round 2: same client, should now allocate a fresh upstream source
	// port because the old session was reaped.
	if _, err := client.Write([]byte("secnd")); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	_ = readUDPWithDeadline(t, client, 5, time.Second)

	if n := sources.distinctCount(); n < 2 {
		t.Fatalf("expected ≥2 distinct upstream source ports after idle reaping, got %d", n)
	}

	cancel()
	<-served
}

// TestServe_UDP_BindFailure surfaces a UDP bind error from Serve.
func TestServe_UDP_BindFailure(t *testing.T) {
	requireUDPListenSupport(t)

	occ, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = occ.Close() })

	specs := []Spec{{Listen: occ.LocalAddr().String(), Upstream: "127.0.0.1:1", Protocol: ProtocolUDP}}
	logger := slog.New(slog.DiscardHandler)

	err = Serve(context.Background(), specs, ServeOptions{DrainTimeout: time.Second}, logger)
	if err == nil {
		t.Fatal("expected bind failure, got nil")
	}
}

// startUDPEchoServer runs a UDP echo on a random port and returns its addr.
func startUDPEchoServer(t *testing.T) string {
	t.Helper()
	addr, _ := startUDPEchoServerWithSourceTracking(t)
	return addr
}

type sourceTracker struct {
	mu      sync.Mutex
	seen    map[string]struct{}
}

func (s *sourceTracker) record(addr net.Addr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = make(map[string]struct{})
	}
	s.seen[addr.String()] = struct{}{}
}

func (s *sourceTracker) distinctCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}

func startUDPEchoServerWithSourceTracking(t *testing.T) (string, *sourceTracker) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	tracker := &sourceTracker{}
	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			tracker.record(addr)
			_, _ = pc.WriteTo(buf[:n], addr)
		}
	}()
	return pc.LocalAddr().String(), tracker
}

func freeUDPAddr(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := pc.LocalAddr().String()
	_ = pc.Close()
	return addr
}

func dialUDP(t *testing.T, addr string) *net.UDPConn {
	t.Helper()
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	c, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return c
}

// writeUDPUntilReady sends until the proxy has bound and the echo
// round-trips. UDP has no accept handshake so we retry through any
// transient packet loss while the server starts.
func writeUDPUntilReady(t *testing.T, c *net.UDPConn, payload []byte, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := c.Write(payload); err != nil {
			t.Fatalf("write: %v", err)
		}
		_ = c.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		buf := make([]byte, len(payload))
		n, _, err := c.ReadFromUDP(buf)
		if err == nil && n == len(payload) {
			// First successful round-trip; rewind the deadline so the
			// caller can do its own ReadFromUDP without a stale one.
			_ = c.SetReadDeadline(time.Time{})
			// Prime the next read by sending again.
			if _, err := c.Write(payload); err != nil {
				t.Fatalf("write: %v", err)
			}
			return
		}
	}
	t.Fatalf("UDP proxy never round-tripped at %s within %v", c.RemoteAddr(), within)
}

func readUDPWithDeadline(t *testing.T, c *net.UDPConn, n int, within time.Duration) string {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(within))
	buf := make([]byte, n)
	got, _, err := c.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(buf[:got])
}

func requireUDPListenSupport(t *testing.T) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err == nil {
		_ = pc.Close()
		return
	}
	if errors.Is(err, syscall.EPERM) || errors.Is(err, os.ErrPermission) {
		t.Skipf("skipping test: UDP listen is not permitted in this environment: %v", err)
	}
	t.Fatalf("probe listen failed: %v", err)
}
