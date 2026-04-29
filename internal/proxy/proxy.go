// Package proxy implements the bulb dataplane: an L4 TCP splice that
// accepts on each configured listen address and forwards to the
// matching upstream (a Service ClusterIP:targetPort).
package proxy

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ProxyProtocolVersion represents the PROXY protocol version to use.
type ProxyProtocolVersion string

const (
	ProxyProtocolVersionNone = ProxyProtocolVersion("")
	ProxyProtocolVersion1    = ProxyProtocolVersion("1")
	ProxyProtocolVersion2    = ProxyProtocolVersion("2")
)

// Run is the `bulb proxy` subcommand entrypoint.
func Run(args []string) error {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	var tcpPairs, udpPairs multiFlag
	fs.Var(&tcpPairs, "upstream", "TCP listen=upstream pair, e.g. 0.0.0.0:8443=10.96.1.5:8443 (repeatable)")
	fs.Var(&udpPairs, "udp-upstream", "UDP listen=upstream pair, e.g. 0.0.0.0:53=10.96.1.5:53 (repeatable)")
	drain := fs.Duration("drain-timeout", 30*time.Second, "max time to wait for in-flight connections after SIGTERM")
	udpIdle := fs.Duration("udp-idle-timeout", 30*time.Second, "tear down UDP session after this much silence on both sides")
	healthAddr := fs.String("health-bind-address", ":8081", "address the proxy liveness/readiness endpoint binds to; empty disables it")
	healthTimeout := fs.Duration("health-check-timeout", 250*time.Millisecond, "timeout for TCP upstream readiness checks")
	proxyProto := fs.String("proxy-protocol", "", "PROXY protocol version to emit upstream: v1, v2, or empty for none")
	var endpointPairs multiFlag
	fs.Var(&endpointPairs, "endpoint", "endpoint upstreams for a listen port, e.g. 0.0.0.0:8080=10.244.1.2:8080,10.244.1.3:8080 (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(tcpPairs) == 0 && len(udpPairs) == 0 {
		return errors.New("at least one --upstream or --udp-upstream pair is required")
	}

	proxyProtocol := ProxyProtocolVersion(*proxyProto)
	if *proxyProto != "" && *proxyProto != "1" && *proxyProto != "2" {
		return fmt.Errorf("invalid --proxy-protocol value %q: must be empty, '1', or '2'", *proxyProto)
	}

	specs, err := parsePairsWithProtocol(tcpPairs, ProtocolTCP)
	if err != nil {
		return err
	}
	udpSpecs, err := parsePairsWithProtocol(udpPairs, ProtocolUDP)
	if err != nil {
		return err
	}
	specs = append(specs, udpSpecs...)

	// Apply proxy protocol to all specs
	for i := range specs {
		specs[i].ProxyProtocol = proxyProtocol
	}

	// Parse endpoint pairs and match them with upstream specs
	endpointMap, err := parseEndpointPairs(endpointPairs)
	if err != nil {
		return fmt.Errorf("parse endpoints: %w", err)
	}
	// Apply endpoints to matching specs
	for i := range specs {
		if endpoints, ok := endpointMap[specs[i].Listen]; ok {
			specs[i].Endpoints = endpoints
		}
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	return Serve(ctx, specs, ServeOptions{
		DrainTimeout:       *drain,
		UDPIdleTimeout:     *udpIdle,
		HealthBindAddress:  *healthAddr,
		HealthCheckTimeout: *healthTimeout,
	}, logger)
}

// Protocol identifies the wire protocol of a Spec.
type Protocol string

const (
	ProtocolTCP Protocol = "tcp"
	ProtocolUDP Protocol = "udp"
)

// Spec is a single listen=upstream forwarding rule. Protocol defaults to
// TCP when empty.
type Spec struct {
	Listen        string
	Upstream      string
	Protocol      Protocol
	ProxyProtocol ProxyProtocolVersion
	// Endpoints is used when externalTrafficPolicy is Local. If non-empty,
	// the proxy dials these endpoint hostports instead of the ClusterIP.
	Endpoints []string
}

// ServeOptions tunes Serve. Zero values are sensible defaults.
type ServeOptions struct {
	// DrainTimeout caps how long Serve waits for in-flight TCP
	// connections after ctx is canceled. Default 30s.
	DrainTimeout time.Duration
	// UDPIdleTimeout tears down a per-client UDP session after this much
	// silence in both directions. Default 30s.
	UDPIdleTimeout time.Duration
	// HealthBindAddress exposes /healthz and /readyz when non-empty.
	// Run defaults this to :8081; direct Serve callers can leave it empty.
	HealthBindAddress string
	// HealthCheckTimeout caps each TCP upstream readiness dial. Default 250ms.
	HealthCheckTimeout time.Duration
}

// Serve binds every spec, accepts (TCP) or reads (UDP), and forwards to
// the matching upstream. It returns when ctx is canceled and either all
// in-flight TCP connections have finished or the drain timeout elapsed.
// UDP sessions tear down on idle timeout regardless.
func Serve(ctx context.Context, specs []Spec, opts ServeOptions, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	if opts.DrainTimeout <= 0 {
		opts.DrainTimeout = 30 * time.Second
	}
	if opts.UDPIdleTimeout <= 0 {
		opts.UDPIdleTimeout = 30 * time.Second
	}
	if opts.HealthCheckTimeout <= 0 {
		opts.HealthCheckTimeout = 250 * time.Millisecond
	}

	var (
		tcpListeners []net.Listener
		tcpSpecs     []Spec
		udpConns     []net.PacketConn
		udpSpecs     []Spec
		conns        sync.WaitGroup
	)
	closeAllOpened := func() {
		closeAll(tcpListeners)
		for _, c := range udpConns {
			_ = c.Close()
		}
	}
	for _, spec := range specs {
		switch spec.protocol() {
		case ProtocolTCP:
			l, err := net.Listen("tcp", spec.Listen)
			if err != nil {
				closeAllOpened()
				return fmt.Errorf("listen tcp %s: %w", spec.Listen, err)
			}
			tcpListeners = append(tcpListeners, l)
			tcpSpecs = append(tcpSpecs, spec)
			logger.Info("listening", "proto", "tcp", "addr", l.Addr().String(), "upstream", spec.Upstream, "proxy_protocol", spec.ProxyProtocol)
		case ProtocolUDP:
			pc, err := net.ListenPacket("udp", spec.Listen)
			if err != nil {
				closeAllOpened()
				return fmt.Errorf("listen udp %s: %w", spec.Listen, err)
			}
			udpConns = append(udpConns, pc)
			udpSpecs = append(udpSpecs, spec)
			logger.Info("listening", "proto", "udp", "addr", pc.LocalAddr().String(), "upstream", spec.Upstream, "endpoints", spec.Endpoints)
		default:
			closeAllOpened()
			return fmt.Errorf("unsupported protocol %q on %s", spec.Protocol, spec.Listen)
		}
	}
	if opts.HealthBindAddress != "" {
		stopHealth, err := startHealthServer(ctx, opts.HealthBindAddress, specs, opts.HealthCheckTimeout, logger)
		if err != nil {
			closeAllOpened()
			return err
		}
		defer stopHealth()
	}

	acceptCtx, cancelAccept := context.WithCancel(ctx)
	defer cancelAccept()

	var accepts sync.WaitGroup
	for i, l := range tcpListeners {
		accepts.Add(1)
		go func(l net.Listener, spec Spec) {
			defer accepts.Done()
			acceptLoop(acceptCtx, l, spec, &conns, logger)
		}(l, tcpSpecs[i])
	}
	udpForwarders := make([]*udpForwarder, 0, len(udpConns))
	for i, pc := range udpConns {
		f := newUDPForwarder(pc, udpSpecs[i], opts.UDPIdleTimeout, logger)
		udpForwarders = append(udpForwarders, f)
		accepts.Add(1)
		go func(f *udpForwarder) {
			defer accepts.Done()
			f.run(acceptCtx)
		}(f)
	}

	<-ctx.Done()
	logger.Info("shutdown initiated", "drain_timeout", opts.DrainTimeout.String())
	cancelAccept()
	closeAll(tcpListeners)
	for _, pc := range udpConns {
		_ = pc.Close()
	}
	accepts.Wait()

	for _, f := range udpForwarders {
		f.closeAllSessions()
	}

	if waitTimeout(&conns, opts.DrainTimeout) {
		logger.Info("drained cleanly")
	} else {
		logger.Warn("drain timeout exceeded; closing remaining connections")
	}
	return nil
}

func (s Spec) protocol() Protocol {
	if s.Protocol == "" {
		return ProtocolTCP
	}
	return s.Protocol
}

func acceptLoop(ctx context.Context, l net.Listener, spec Spec, conns *sync.WaitGroup, logger *slog.Logger) {
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			logger.Error("accept failed", "addr", l.Addr().String(), "err", err.Error())
			continue
		}
		conns.Add(1)
		go func() {
			defer conns.Done()
			handle(ctx, c, spec, logger)
		}()
	}
}

// endpointCounter tracks the next endpoint index to use for round-robin.
// Key is the listen address.
var endpointCounters sync.Map

func handle(ctx context.Context, client net.Conn, spec Spec, logger *slog.Logger) {
	defer client.Close()

	upstream := upstreamFor(spec)
	dialer := net.Dialer{Timeout: 5 * time.Second}
	server, err := dialer.DialContext(ctx, "tcp", upstream)
	if err != nil {
		logger.Error("upstream dial failed", "upstream", upstream, "err", err.Error())
		return
	}
	defer server.Close()

	stop := context.AfterFunc(ctx, func() {
		_ = client.SetDeadline(time.Now())
		_ = server.SetDeadline(time.Now())
	})
	defer stop()

	// Write PROXY protocol header if configured
	if spec.ProxyProtocol != "" {
		if err := writeProxyHeader(client, server, spec.ProxyProtocol, logger); err != nil {
			logger.Error("failed to write PROXY header", "upstream", upstream, "err", err.Error())
			return
		}
	}

	splice(client, server)
}

// upstreamFor returns the ClusterIP upstream or the next endpoint hostport.
func upstreamFor(spec Spec) string {
	if len(spec.Endpoints) == 0 {
		return spec.Upstream
	}
	key := string(spec.protocol()) + ":" + spec.Listen
	val, _ := endpointCounters.LoadOrStore(key, &atomicCounter{})
	counter := val.(*atomicCounter)
	idx := counter.Inc() % uint64(len(spec.Endpoints))
	return spec.Endpoints[idx]
}

// atomicCounter is a thread-safe counter.
type atomicCounter struct {
	count uint64
}

func (c *atomicCounter) Inc() uint64 {
	return atomic.AddUint64(&c.count, 1) - 1
}

// splice copies bytes in both directions and propagates half-close so
// upstream/downstream can each signal EOF independently.
func splice(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		copyAndCloseWrite(a, b)
	}()
	go func() {
		defer wg.Done()
		copyAndCloseWrite(b, a)
	}()
	wg.Wait()
}

// copyAndCloseWrite copies src→dst and then half-closes dst's write side.
// Both directions running concurrently propagate EOF correctly.
func copyAndCloseWrite(dst, src net.Conn) {
	type closeWriter interface{ CloseWrite() error }
	defer func() {
		if cw, ok := dst.(closeWriter); ok {
			_ = cw.CloseWrite()
		}
	}()
	buf := bufPool.Get().(*[]byte)
	defer bufPool.Put(buf)
	_, _ = copyBuffer(dst, src, *buf)
}

// copyBuffer is a thin wrapper around io.CopyBuffer kept for testability.
func copyBuffer(dst net.Conn, src net.Conn, buf []byte) (int64, error) {
	var written int64
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			w, werr := dst.Write(buf[:n])
			written += int64(w)
			if werr != nil {
				return written, werr
			}
		}
		if rerr != nil {
			return written, rerr
		}
	}
}

var bufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 32*1024)
		return &b
	},
}

func closeAll(ls []net.Listener) {
	for _, l := range ls {
		_ = l.Close()
	}
}

// writeProxyHeader writes a PROXY protocol v1 or v2 header to the upstream
// connection. The header includes the client's source address and the
// proxy's destination address (the listen address).
func writeProxyHeader(client, server net.Conn, version ProxyProtocolVersion, logger *slog.Logger) error {
	clientAddr := client.RemoteAddr().(*net.TCPAddr)
	proxyAddr := client.LocalAddr().(*net.TCPAddr)

	switch version {
	case ProxyProtocolVersion1:
		header := buildProxyV1Header(clientAddr, proxyAddr)
		if _, err := server.Write([]byte(header)); err != nil {
			return fmt.Errorf("write PROXY v1 header: %w", err)
		}
	case ProxyProtocolVersion2:
		header := buildProxyV2Header(clientAddr, proxyAddr)
		if _, err := server.Write(header); err != nil {
			return fmt.Errorf("write PROXY v2 header: %w", err)
		}
	default:
		return fmt.Errorf("unsupported PROXY protocol version: %s", version)
	}
	return nil
}

// buildProxyV1Header constructs a PROXY protocol v1 header.
// Format: "PROXY <INET6|INET> <src_ip> <dst_ip> <src_port> <dst_port>\r\n"
func buildProxyV1Header(src, dst *net.TCPAddr) string {
	IAL := "INET"
	if src.IP.To4() == nil {
		IAL = "INET6"
	}
	return fmt.Sprintf("PROXY %s %s %s %d %d\r\n",
		IAL, src.IP.String(), dst.IP.String(), src.Port, dst.Port)
}

// buildProxyV2Header constructs a PROXY protocol v2 header.
// Header format per spec:
// signature (12 bytes) + version/cmd (1 byte) + family/protocol (1 byte) + length (2 bytes) + payload + CR+LF
func buildProxyV2Header(src, dst *net.TCPAddr) []byte {
	var buf bytes.Buffer

	// PROXY protocol v2 signature: "\r\n\r\n\x00\r\nQUIT\n"
	buf.WriteString("\x0D\x0A\x0D\x0A\x00\x0D\x0A\x51\x55\x49\x54\x0A")

	// Version 2 (high nibble = 0x2) + Command PROXY (low nibble = 0x1) = 0x21
	buf.WriteByte(0x21)

	// Transport protocol: TCP over IPv4 (0x11) or TCP over IPv6 (0x21)
	// family (high nibble) + protocol (low nibble)
	// INET = 0x1, INET6 = 0x2, STREAM (TCP) = 0x1
	if src.IP.To4() == nil {
		// TCP over IPv6: family=0x2, protocol=0x1 -> 0x21
		buf.WriteByte(0x21)
	} else {
		// TCP over IPv4: family=0x1, protocol=0x1 -> 0x11
		buf.WriteByte(0x11)
	}

	// Length of the following payload (src_ip + dst_ip + src_port + dst_port)
	// IPv4: 4 + 4 + 2 + 2 = 12 bytes
	// IPv6: 16 + 16 + 2 + 2 = 36 bytes
	var payloadLen uint16
	if src.IP.To4() == nil {
		payloadLen = 36
	} else {
		payloadLen = 12
	}
	buf.WriteByte(byte(payloadLen >> 8))
	buf.WriteByte(byte(payloadLen & 0xFF))

	// Source address
	if src.IP.To4() != nil {
		buf.Write(src.IP.To4())
	} else {
		buf.Write(src.IP.To16())
	}

	// Destination address
	if dst.IP.To4() != nil {
		buf.Write(dst.IP.To4())
	} else {
		buf.Write(dst.IP.To16())
	}

	// Source port (2 bytes, big-endian)
	buf.WriteByte(byte(src.Port >> 8))
	buf.WriteByte(byte(src.Port & 0xFF))

	// Destination port (2 bytes, big-endian)
	buf.WriteByte(byte(dst.Port >> 8))
	buf.WriteByte(byte(dst.Port & 0xFF))

	// CR+LF to end the header
	buf.WriteString("\r\n")

	return buf.Bytes()
}

// waitTimeout returns true if wg finishes before timeout, false otherwise.
func waitTimeout(wg *sync.WaitGroup, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// multiFlag accumulates repeated --upstream flag values.
type multiFlag []string

func (m *multiFlag) String() string     { return fmt.Sprint([]string(*m)) }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func parsePairsWithProtocol(raw []string, proto Protocol) ([]Spec, error) {
	specs := make([]Spec, 0, len(raw))
	for _, r := range raw {
		spec, err := parsePair(r)
		if err != nil {
			return nil, err
		}
		spec.Protocol = proto
		specs = append(specs, spec)
	}
	return specs, nil
}

// parseEndpointPairs parses --endpoint flags like
// "0.0.0.0:8080=10.244.1.2:8080,10.244.1.3:8080" or
// "[::]:80=[2001:db8::1]:80" and returns listen address -> endpoint hostports.
func parseEndpointPairs(raw []string) (map[string][]string, error) {
	result := make(map[string][]string)
	for _, r := range raw {
		idx := strings.IndexByte(r, '=')
		if idx < 0 {
			return nil, fmt.Errorf("invalid endpoint pair %q: expected listen=endpoints[,...]", r)
		}
		listen, endpoints := r[:idx], r[idx+1:]
		if listen == "" || endpoints == "" {
			return nil, fmt.Errorf("invalid endpoint pair %q: empty listen or endpoints", r)
		}
		if _, _, err := net.SplitHostPort(listen); err != nil {
			return nil, fmt.Errorf("invalid listen %q in endpoint pair: %w", listen, err)
		}
		rawEndpoints := strings.Split(endpoints, ",")
		filtered := make([]string, 0, len(rawEndpoints))
		for _, endpoint := range rawEndpoints {
			trimmed := strings.TrimSpace(endpoint)
			if trimmed == "" {
				continue
			}
			if _, _, err := net.SplitHostPort(trimmed); err != nil {
				return nil, fmt.Errorf("invalid endpoint %q in endpoint pair: %w", trimmed, err)
			}
			filtered = append(filtered, trimmed)
		}
		if len(filtered) == 0 {
			return nil, fmt.Errorf("invalid endpoint pair %q: no endpoints", r)
		}
		result[listen] = filtered
	}
	return result, nil
}

func parsePair(s string) (Spec, error) {
	for i := 0; i < len(s); i++ {
		if s[i] == '=' {
			listen, upstream := s[:i], s[i+1:]
			if listen == "" || upstream == "" {
				return Spec{}, fmt.Errorf("invalid pair %q: empty listen or upstream", s)
			}
			if _, _, err := net.SplitHostPort(listen); err != nil {
				return Spec{}, fmt.Errorf("invalid listen %q: %w", listen, err)
			}
			if _, _, err := net.SplitHostPort(upstream); err != nil {
				return Spec{}, fmt.Errorf("invalid upstream %q: %w", upstream, err)
			}
			return Spec{Listen: listen, Upstream: upstream}, nil
		}
	}
	return Spec{}, fmt.Errorf("invalid pair %q: expected listen=upstream", s)
}
