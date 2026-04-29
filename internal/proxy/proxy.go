// Package proxy implements the bulb dataplane: an L4 TCP splice that
// accepts on each configured listen address and forwards to the
// matching upstream (a Service ClusterIP:targetPort).
package proxy

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// Run is the `bulb proxy` subcommand entrypoint.
func Run(args []string) error {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	var tcpPairs, udpPairs multiFlag
	fs.Var(&tcpPairs, "upstream", "TCP listen=upstream pair, e.g. 0.0.0.0:8443=10.96.1.5:8443 (repeatable)")
	fs.Var(&udpPairs, "udp-upstream", "UDP listen=upstream pair, e.g. 0.0.0.0:53=10.96.1.5:53 (repeatable)")
	drain := fs.Duration("drain-timeout", 30*time.Second, "max time to wait for in-flight connections after SIGTERM")
	udpIdle := fs.Duration("udp-idle-timeout", 30*time.Second, "tear down UDP session after this much silence on both sides")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(tcpPairs) == 0 && len(udpPairs) == 0 {
		return errors.New("at least one --upstream or --udp-upstream pair is required")
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

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	return Serve(ctx, specs, ServeOptions{DrainTimeout: *drain, UDPIdleTimeout: *udpIdle}, logger)
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
	Listen   string
	Upstream string
	Protocol Protocol
}

// ServeOptions tunes Serve. Zero values are sensible defaults.
type ServeOptions struct {
	// DrainTimeout caps how long Serve waits for in-flight TCP
	// connections after ctx is canceled. Default 30s.
	DrainTimeout time.Duration
	// UDPIdleTimeout tears down a per-client UDP session after this much
	// silence in both directions. Default 30s.
	UDPIdleTimeout time.Duration
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

	var (
		tcpListeners []net.Listener
		tcpUpstreams []string
		udpConns     []net.PacketConn
		udpUpstreams []string
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
			tcpUpstreams = append(tcpUpstreams, spec.Upstream)
			logger.Info("listening", "proto", "tcp", "addr", l.Addr().String(), "upstream", spec.Upstream)
		case ProtocolUDP:
			pc, err := net.ListenPacket("udp", spec.Listen)
			if err != nil {
				closeAllOpened()
				return fmt.Errorf("listen udp %s: %w", spec.Listen, err)
			}
			udpConns = append(udpConns, pc)
			udpUpstreams = append(udpUpstreams, spec.Upstream)
			logger.Info("listening", "proto", "udp", "addr", pc.LocalAddr().String(), "upstream", spec.Upstream)
		default:
			closeAllOpened()
			return fmt.Errorf("unsupported protocol %q on %s", spec.Protocol, spec.Listen)
		}
	}

	acceptCtx, cancelAccept := context.WithCancel(ctx)
	defer cancelAccept()

	var accepts sync.WaitGroup
	for i, l := range tcpListeners {
		accepts.Add(1)
		go func(l net.Listener, upstream string) {
			defer accepts.Done()
			acceptLoop(acceptCtx, l, upstream, &conns, logger)
		}(l, tcpUpstreams[i])
	}
	udpForwarders := make([]*udpForwarder, 0, len(udpConns))
	for i, pc := range udpConns {
		f := newUDPForwarder(pc, udpUpstreams[i], opts.UDPIdleTimeout, logger)
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

func acceptLoop(ctx context.Context, l net.Listener, upstream string, conns *sync.WaitGroup, logger *slog.Logger) {
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
			handle(ctx, c, upstream, logger)
		}()
	}
}

func handle(ctx context.Context, client net.Conn, upstream string, logger *slog.Logger) {
	defer client.Close()

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

	splice(client, server)
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
