package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// udpForwarder serves a single frontend listener and fans packets out
// to per-client upstream sockets. It's the UDP analogue of a TCP
// listen+accept loop, except UDP has no connection so we synthesize
// "sessions" keyed on the client's source address.
//
// One frontend socket → many session sockets (one per unique client
// addr). Each session has an idle timer; when it expires the upstream
// socket and the session entry are dropped, releasing the ephemeral
// port and the goroutine.
type udpForwarder struct {
	listen      net.PacketConn
	upstream    string
	idleTimeout time.Duration
	logger      *slog.Logger

	mu       sync.Mutex
	sessions map[string]*udpSession
}

type udpSession struct {
	upstream     *net.UDPConn
	clientAddr   net.Addr
	lastActivity atomic.Int64 // unix nano
	closeOnce    sync.Once
}

func newUDPForwarder(listen net.PacketConn, upstream string, idle time.Duration, logger *slog.Logger) *udpForwarder {
	return &udpForwarder{
		listen:      listen,
		upstream:    upstream,
		idleTimeout: idle,
		logger:      logger,
		sessions:    make(map[string]*udpSession),
	}
}

// run reads from the frontend socket and dispatches packets to the
// matching session, creating new sessions and an idle reaper as needed.
// Returns when the frontend socket is closed.
func (f *udpForwarder) run(ctx context.Context) {
	reaperDone := make(chan struct{})
	go f.reapLoop(ctx, reaperDone)
	defer func() {
		<-reaperDone
	}()

	buf := make([]byte, 64*1024) // max UDP datagram size
	for {
		n, clientAddr, err := f.listen.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			f.logger.Error("udp read failed", "addr", f.listen.LocalAddr().String(), "err", err.Error())
			continue
		}

		sess, err := f.getOrCreate(clientAddr)
		if err != nil {
			f.logger.Error("udp upstream dial failed", "upstream", f.upstream, "client", clientAddr.String(), "err", err.Error())
			continue
		}
		sess.lastActivity.Store(time.Now().UnixNano())

		if _, werr := sess.upstream.Write(buf[:n]); werr != nil {
			f.logger.Error("udp upstream write failed", "upstream", f.upstream, "err", werr.Error())
			f.dropSession(clientAddr.String())
		}
	}
}

func (f *udpForwarder) getOrCreate(clientAddr net.Addr) (*udpSession, error) {
	key := clientAddr.String()

	f.mu.Lock()
	if sess, ok := f.sessions[key]; ok {
		f.mu.Unlock()
		return sess, nil
	}
	f.mu.Unlock()

	upstreamAddr, err := net.ResolveUDPAddr("udp", f.upstream)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialUDP("udp", nil, upstreamAddr)
	if err != nil {
		return nil, err
	}

	sess := &udpSession{
		upstream:   conn,
		clientAddr: clientAddr,
	}
	sess.lastActivity.Store(time.Now().UnixNano())

	f.mu.Lock()
	if existing, ok := f.sessions[key]; ok {
		// Lost the race; drop the one we just created.
		f.mu.Unlock()
		_ = conn.Close()
		return existing, nil
	}
	f.sessions[key] = sess
	f.mu.Unlock()

	go f.upstreamReadLoop(sess, key)
	return sess, nil
}

// upstreamReadLoop pumps replies from upstream back to the client via
// the frontend socket. Returns when the upstream socket is closed.
func (f *udpForwarder) upstreamReadLoop(sess *udpSession, key string) {
	buf := make([]byte, 64*1024)
	for {
		n, _, err := sess.upstream.ReadFromUDP(buf)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				f.logger.Debug("udp upstream read closed", "client", key, "err", err.Error())
			}
			f.dropSession(key)
			return
		}
		sess.lastActivity.Store(time.Now().UnixNano())
		if _, werr := f.listen.WriteTo(buf[:n], sess.clientAddr); werr != nil {
			f.logger.Error("udp client write failed", "client", key, "err", werr.Error())
			f.dropSession(key)
			return
		}
	}
}

// reapLoop tears down sessions whose lastActivity is older than
// idleTimeout. Runs until ctx is canceled.
func (f *udpForwarder) reapLoop(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	interval := f.idleTimeout / 2
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			f.reap(now)
		}
	}
}

func (f *udpForwarder) reap(now time.Time) {
	cutoff := now.Add(-f.idleTimeout).UnixNano()
	var stale []string
	f.mu.Lock()
	for key, sess := range f.sessions {
		if sess.lastActivity.Load() < cutoff {
			stale = append(stale, key)
		}
	}
	f.mu.Unlock()
	for _, key := range stale {
		f.dropSession(key)
	}
}

func (f *udpForwarder) dropSession(key string) {
	f.mu.Lock()
	sess, ok := f.sessions[key]
	if ok {
		delete(f.sessions, key)
	}
	f.mu.Unlock()
	if !ok {
		return
	}
	sess.closeOnce.Do(func() {
		_ = sess.upstream.Close()
	})
}

// closeAllSessions tears down every session — called on shutdown so
// the upstreamReadLoop goroutines exit before Serve returns.
func (f *udpForwarder) closeAllSessions() {
	f.mu.Lock()
	keys := make([]string, 0, len(f.sessions))
	for k := range f.sessions {
		keys = append(keys, k)
	}
	f.mu.Unlock()
	for _, k := range keys {
		f.dropSession(k)
	}
}
