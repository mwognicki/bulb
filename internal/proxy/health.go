package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

func startHealthServer(ctx context.Context, addr string, specs []Spec, timeout time.Duration, logger *slog.Logger) (func(), error) {
	mux := http.NewServeMux()
	checker := &healthChecker{
		specs:   append([]Spec(nil), specs...),
		timeout: timeout,
	}
	mux.HandleFunc("/healthz", checker.healthz)
	mux.HandleFunc("/readyz", checker.readyz)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen health %s: %w", addr, err)
	}

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 2 * time.Second,
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.Error("health server failed", "addr", addr, "err", err.Error())
		}
	}()

	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdownCtx)
			<-done
		})
	}
	go func() {
		<-ctx.Done()
		stop()
	}()

	logger.Info("health probes listening", "addr", ln.Addr().String())
	return stop, nil
}

type healthChecker struct {
	specs   []Spec
	timeout time.Duration
}

func (h *healthChecker) healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (h *healthChecker) readyz(w http.ResponseWriter, r *http.Request) {
	for _, spec := range h.specs {
		if spec.protocol() != ProtocolTCP {
			continue
		}
		upstream := readinessUpstream(spec)
		dialer := net.Dialer{Timeout: h.timeout}
		conn, err := dialer.DialContext(r.Context(), "tcp", upstream)
		if err != nil {
			http.Error(w, fmt.Sprintf("tcp upstream %s for listener %s is not ready: %v", upstream, spec.Listen, err), http.StatusServiceUnavailable)
			return
		}
		_ = conn.Close()
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready\n"))
}

func readinessUpstream(spec Spec) string {
	if len(spec.Endpoints) > 0 {
		return spec.Endpoints[0]
	}
	return spec.Upstream
}
