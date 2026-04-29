package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
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
			if got.Listen != tc.want.Listen || got.Upstream != tc.want.Upstream || got.Protocol != tc.want.Protocol {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseEndpointPairs(t *testing.T) {
	got, err := parseEndpointPairs([]string{
		"0.0.0.0:8443=10.244.1.7:9443,10.244.1.8:9443",
		"[::]:8443=[fd00::7]:9443",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got["0.0.0.0:8443"]) != 2 || got["0.0.0.0:8443"][0] != "10.244.1.7:9443" {
		t.Fatalf("unexpected v4 endpoints: %+v", got)
	}
	if len(got["[::]:8443"]) != 1 || got["[::]:8443"][0] != "[fd00::7]:9443" {
		t.Fatalf("unexpected v6 endpoints: %+v", got)
	}
}

func TestUpstreamForRoundRobinsEndpoints(t *testing.T) {
	spec := Spec{
		Listen:    "127.0.0.1:8443",
		Upstream:  "10.96.1.5:8443",
		Endpoints: []string{"10.244.1.7:9443", "10.244.1.8:9443"},
	}
	if got := upstreamFor(spec); got != "10.244.1.7:9443" {
		t.Fatalf("first upstream: got %q", got)
	}
	if got := upstreamFor(spec); got != "10.244.1.8:9443" {
		t.Fatalf("second upstream: got %q", got)
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

func TestServe_HealthEndpointsReadyWhenTCPUpstreamReachable(t *testing.T) {
	requireTCPListenSupport(t)

	upstream := startEchoServer(t)
	listenAddr := freeAddr(t)
	healthAddr := freeAddr(t)
	specs := []Spec{{Listen: listenAddr, Upstream: upstream}}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	logger := slog.New(slog.DiscardHandler)
	served := make(chan error, 1)
	go func() {
		served <- Serve(ctx, specs, ServeOptions{
			DrainTimeout:       time.Second,
			HealthBindAddress:  healthAddr,
			HealthCheckTimeout: 100 * time.Millisecond,
		}, logger)
	}()

	if got := getHTTPStatusUntilReady(t, "http://"+healthAddr+"/healthz", time.Second); got != http.StatusOK {
		t.Fatalf("/healthz: got %d want 200", got)
	}
	if got := getHTTPStatusUntilReady(t, "http://"+healthAddr+"/readyz", time.Second); got != http.StatusOK {
		t.Fatalf("/readyz: got %d want 200", got)
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

func TestServe_ReadyzFailsWhenTCPUpstreamUnreachable(t *testing.T) {
	requireTCPListenSupport(t)

	listenAddr := freeAddr(t)
	healthAddr := freeAddr(t)
	specs := []Spec{{Listen: listenAddr, Upstream: "127.0.0.1:1"}}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	logger := slog.New(slog.DiscardHandler)
	served := make(chan error, 1)
	go func() {
		served <- Serve(ctx, specs, ServeOptions{
			DrainTimeout:       time.Second,
			HealthBindAddress:  healthAddr,
			HealthCheckTimeout: 50 * time.Millisecond,
		}, logger)
	}()

	if got := getHTTPStatusUntilReady(t, "http://"+healthAddr+"/healthz", time.Second); got != http.StatusOK {
		t.Fatalf("/healthz: got %d want 200", got)
	}
	if got := getHTTPStatusUntilReady(t, "http://"+healthAddr+"/readyz", time.Second); got != http.StatusServiceUnavailable {
		t.Fatalf("/readyz: got %d want 503", got)
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

func TestServe_ReadyzUsesLocalEndpointWhenConfigured(t *testing.T) {
	requireTCPListenSupport(t)

	endpoint := startEchoServer(t)
	listenAddr := freeAddr(t)
	healthAddr := freeAddr(t)
	specs := []Spec{{
		Listen:    listenAddr,
		Upstream:  "127.0.0.1:1",
		Endpoints: []string{endpoint},
	}}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	logger := slog.New(slog.DiscardHandler)
	served := make(chan error, 1)
	go func() {
		served <- Serve(ctx, specs, ServeOptions{
			DrainTimeout:       time.Second,
			HealthBindAddress:  healthAddr,
			HealthCheckTimeout: 100 * time.Millisecond,
		}, logger)
	}()

	if got := getHTTPStatusUntilReady(t, "http://"+healthAddr+"/readyz", time.Second); got != http.StatusOK {
		t.Fatalf("/readyz: got %d want 200", got)
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

func TestServe_ReadyzForUDPOnlyDoesNotProbeUpstream(t *testing.T) {
	requireUDPListenSupport(t)
	requireTCPListenSupport(t)

	listenAddr := freeUDPAddr(t)
	healthAddr := freeAddr(t)
	specs := []Spec{{Listen: listenAddr, Upstream: "127.0.0.1:1", Protocol: ProtocolUDP}}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	logger := slog.New(slog.DiscardHandler)
	served := make(chan error, 1)
	go func() {
		served <- Serve(ctx, specs, ServeOptions{
			DrainTimeout:       time.Second,
			HealthBindAddress:  healthAddr,
			HealthCheckTimeout: 50 * time.Millisecond,
		}, logger)
	}()

	if got := getHTTPStatusUntilReady(t, "http://"+healthAddr+"/readyz", time.Second); got != http.StatusOK {
		t.Fatalf("/readyz: got %d want 200", got)
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

func getHTTPStatusUntilReady(t *testing.T, url string, within time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(within)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			return resp.StatusCode
		}
		lastErr = err
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("HTTP endpoint never came up at %s: %v", url, lastErr)
	return 0
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

// startProxyProtocolUpstream runs a TCP server that expects and parses
// PROXY protocol headers before echoing back the payload.
func startProxyProtocolUpstream(t *testing.T) (addr string, rxHeader <-chan string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr = l.Addr().String()
	rxHeaderChan := make(chan string, 1)

	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				// Read PROXY v1 header line (read until \r\n)
				buf := make([]byte, 1024)
				readBytes := 0
				for {
					n, err := c.Read(buf[readBytes : readBytes+1])
					if err != nil {
						return
					}
					readBytes += n
					// Check if we have \r\n
					if readBytes >= 2 {
						data := buf[:readBytes]
						// Check if the last two bytes are \r\n
						if data[readBytes-2] == '\r' && data[readBytes-1] == '\n' {
							header := string(data)
							select {
							case rxHeaderChan <- header:
							default:
							}
							break
						}
					}
					if readBytes >= len(buf) {
						return // header too long
					}
				}
				// Echo back whatever follows (the remaining data + future reads)
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()

	t.Cleanup(func() { _ = l.Close() })
	return addr, rxHeaderChan
}

// TestProxyProtocolV1 verifies that PROXY protocol v1 headers are written
// to the upstream connection before the actual payload.
func TestProxyProtocolV1(t *testing.T) {
	requireTCPListenSupport(t)

	upstreamAddr, rxHeader := startProxyProtocolUpstream(t)

	listenAddr := freeAddr(t)
	specs := []Spec{{
		Listen:        listenAddr,
		Upstream:      upstreamAddr,
		ProxyProtocol: ProxyProtocolVersion1,
	}}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	logger := slog.New(slog.DiscardHandler)
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, specs, ServeOptions{DrainTimeout: time.Second}, logger) }()

	conn := dialUntilReady(t, listenAddr, time.Second)
	t.Cleanup(func() { _ = conn.Close() })

	const payload = "test-proxy-v1"
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Wait for the header to be received by upstream
	select {
	case header := <-rxHeader:
		if !strings.Contains(header, "PROXY INET") {
			t.Fatalf("expected PROXY v1 header, got: %q", header)
		}
		// Verify it ends with \r\n
		if !strings.HasSuffix(header, "\r\n") {
			t.Fatalf("PROXY v1 header should end with \\r\\n, got: %q", header)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for PROXY header")
	}

	// Read echo back (the payload after the header)
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

// TestProxyProtocolV2 verifies that PROXY protocol v2 headers are written
// to the upstream connection. We verify at the unit level - the header
// building functions are tested separately.
func TestProxyProtocolV2_Integration(t *testing.T) {
	requireTCPListenSupport(t)

	// For the integration test, we just verify that the connection works
	// end-to-end with v2 enabled. The header parsing is verified in unit tests.
	upstream := startEchoServer(t)

	listenAddr := freeAddr(t)
	specs := []Spec{{
		Listen:        listenAddr,
		Upstream:      upstream,
		ProxyProtocol: ProxyProtocolVersion2,
	}}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	logger := slog.New(slog.DiscardHandler)
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, specs, ServeOptions{DrainTimeout: time.Second}, logger) }()

	conn := dialUntilReady(t, listenAddr, time.Second)
	t.Cleanup(func() { _ = conn.Close() })

	const payload = "test-proxy-v2"
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The echo server will send back the binary PROXY v2 header + payload.
	// We just verify we get something back (connection works).
	// The exact parsing is done in the unit tests.
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// We should receive at least the PROXY v2 header (16+ bytes) + payload
	if n < 16 {
		t.Fatalf("received too few bytes: %d", n)
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

// TestProxyProtocolNone verifies that without proxy protocol configured,
// no header is prepended to the upstream connection.
func TestProxyProtocolNone(t *testing.T) {
	requireTCPListenSupport(t)

	upstream := startEchoServer(t)

	listenAddr := freeAddr(t)
	specs := []Spec{{
		Listen:        listenAddr,
		Upstream:      upstream,
		ProxyProtocol: "",
	}}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	logger := slog.New(slog.DiscardHandler)
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, specs, ServeOptions{DrainTimeout: time.Second}, logger) }()

	conn := dialUntilReady(t, listenAddr, time.Second)
	t.Cleanup(func() { _ = conn.Close() })

	const payload = "test-no-proxy"
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Without PROXY protocol, the payload should be echoed back exactly
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

// TestBuildProxyV1Header verifies the buildProxyV1Header function.
func TestBuildProxyV1Header(t *testing.T) {
	src := &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 12345}
	dst := &net.TCPAddr{IP: net.ParseIP("192.0.2.2"), Port: 8080}

	header := buildProxyV1Header(src, dst)
	expected := "PROXY INET 192.0.2.1 192.0.2.2 12345 8080\r\n"
	if header != expected {
		t.Fatalf("got %q, want %q", header, expected)
	}
}

// TestBuildProxyV1HeaderIPv6 verifies PROXY v1 header with IPv6 addresses.
func TestBuildProxyV1HeaderIPv6(t *testing.T) {
	src := &net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 12345}
	dst := &net.TCPAddr{IP: net.ParseIP("2001:db8::2"), Port: 8080}

	header := buildProxyV1Header(src, dst)
	expected := "PROXY INET6 2001:db8::1 2001:db8::2 12345 8080\r\n"
	if header != expected {
		t.Fatalf("got %q, want %q", header, expected)
	}
}

// TestBuildProxyV2Header verifies the binary structure of PROXY v2 header.
func TestBuildProxyV2Header(t *testing.T) {
	src := &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 0x1234}
	dst := &net.TCPAddr{IP: net.ParseIP("192.0.2.2"), Port: 0x5678}

	header := buildProxyV2Header(src, dst)

	// Check minimum length: signature(12) + ver_cmd(1) + family_proto(1) + len(2) + payload(12) + crlf(2) = 30
	if len(header) < 30 {
		t.Fatalf("header too short: %d bytes", len(header))
	}

	// Verify signature: "\r\n\r\n\x00\r\nQUIT\n"
	signature := string(header[:12])
	expectedSig := "\x0D\x0A\x0D\x0A\x00\x0D\x0A\x51\x55\x49\x54\x0A"
	if signature != expectedSig {
		t.Fatalf("signature mismatch: got %q, want %q", signature, expectedSig)
	}

	// Verify version+command byte (0x21 = v2 + PROXY command)
	if header[12] != 0x21 {
		t.Fatalf("version+command: got 0x%02x, want 0x21", header[12])
	}

	// Verify family+protocol byte (0x11 = INET + STREAM)
	if header[13] != 0x11 {
		t.Fatalf("family+protocol: got 0x%02x, want 0x11", header[13])
	}

	// Verify length field (12 bytes for IPv4 payload)
	if header[14] != 0x00 || header[15] != 0x0C {
		t.Fatalf("length field: got 0x%02x%02x, want 0x000C", header[14], header[15])
	}

	// Verify source IP is at correct offset
	srcIPStart := 16
	srcIP := net.IP(header[srcIPStart : srcIPStart+4])
	if !srcIP.Equal(net.ParseIP("192.0.2.1")) {
		t.Fatalf("source IP: got %s, want 192.0.2.1", srcIP)
	}
}

// TestBuildProxyV2HeaderIPv6 verifies PROXY v2 header with IPv6 addresses.
func TestBuildProxyV2HeaderIPv6(t *testing.T) {
	src := &net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 0x1234}
	dst := &net.TCPAddr{IP: net.ParseIP("2001:db8::2"), Port: 0x5678}

	header := buildProxyV2Header(src, dst)

	// For IPv6, length should be 36 bytes (16+16+2+2)
	// Header: signature(12) + ver_cmd(1) + family_proto(1) + len(2) + payload(36) + crlf(2) = 54
	if len(header) < 54 {
		t.Fatalf("IPv6 header too short: %d bytes", len(header))
	}

	// Verify family+protocol byte (0x21 = INET6 + STREAM)
	if header[13] != 0x21 {
		t.Fatalf("family+protocol: got 0x%02x, want 0x21", header[13])
	}

	// Verify length field (36 bytes for IPv6 payload)
	if header[14] != 0x00 || header[15] != 0x24 {
		t.Fatalf("length field: got 0x%02x%02x, want 0x0024", header[14], header[15])
	}
}
