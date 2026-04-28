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
	var pairs multiFlag
	fs.Var(&pairs, "upstream", "listen=upstream pair, e.g. 0.0.0.0:8443=10.96.1.5:8443 (repeatable)")
	drain := fs.Duration("drain-timeout", 30*time.Second, "max time to wait for in-flight connections after SIGTERM")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(pairs) == 0 {
		return errors.New("at least one --upstream listen=upstream pair is required")
	}

	specs, err := parsePairs(pairs)
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	return Serve(ctx, specs, *drain, logger)
}

// Spec is a single listen=upstream forwarding rule.
type Spec struct {
	Listen   string
	Upstream string
}

// Serve binds every spec, accepts connections, and splices them to the
// matching upstream. It returns when ctx is canceled and either all
// in-flight connections have finished or the drain timeout elapsed.
func Serve(ctx context.Context, specs []Spec, drainTimeout time.Duration, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}

	var (
		listeners []net.Listener
		conns     sync.WaitGroup
	)
	for _, spec := range specs {
		l, err := net.Listen("tcp", spec.Listen)
		if err != nil {
			closeAll(listeners)
			return fmt.Errorf("listen %s: %w", spec.Listen, err)
		}
		listeners = append(listeners, l)
		logger.Info("listening", "addr", l.Addr().String(), "upstream", spec.Upstream)
	}

	acceptCtx, cancelAccept := context.WithCancel(ctx)
	defer cancelAccept()

	var accepts sync.WaitGroup
	for i, l := range listeners {
		accepts.Add(1)
		go func(l net.Listener, upstream string) {
			defer accepts.Done()
			acceptLoop(acceptCtx, l, upstream, &conns, logger)
		}(l, specs[i].Upstream)
	}

	<-ctx.Done()
	logger.Info("shutdown initiated", "drain_timeout", drainTimeout.String())
	closeAll(listeners)
	accepts.Wait()

	if waitTimeout(&conns, drainTimeout) {
		logger.Info("drained cleanly")
	} else {
		logger.Warn("drain timeout exceeded; closing remaining connections")
	}
	return nil
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

func parsePairs(raw []string) ([]Spec, error) {
	specs := make([]Spec, 0, len(raw))
	for _, r := range raw {
		spec, err := parsePair(r)
		if err != nil {
			return nil, err
		}
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
