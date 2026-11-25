// Package lb is the L4 load balancer under test.
//
// It accepts TCP connections and UDP datagrams that start with a PROXY v1
// line (added by the switch, carrying the real client address), chooses a
// backend with the configured policy, and forwards bytes without looking at
// the application payload. Every decision is logged for offline analysis.
package lb

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"acnlb/internal/policy"
	"acnlb/internal/pool"
	"acnlb/internal/proxyline"
)

type Config struct {
	Policy      policy.Policy
	Pool        *pool.Pool
	LogDir      string
	DialTimeout time.Duration
	UDPTimeout  time.Duration
}

type LB struct {
	cfg Config

	// pickMu makes "choose a backend and count the connection" atomic, so
	// least-loaded sees an exact connection count.
	pickMu sync.Mutex
	last   map[string]string // client IP -> backend it was last sent to

	logMu   sync.Mutex
	decLog  *bufio.Writer
	poolLog *bufio.Writer
	files   []*os.File
}

func New(cfg Config) (*LB, error) {
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 500 * time.Millisecond
	}
	if cfg.UDPTimeout == 0 {
		cfg.UDPTimeout = time.Second
	}
	l := &LB{cfg: cfg, last: make(map[string]string)}

	if err := os.MkdirAll(cfg.LogDir, 0o755); err != nil {
		return nil, err
	}
	dec, err := os.Create(cfg.LogDir + "/lb.csv")
	if err != nil {
		return nil, err
	}
	pl, err := os.Create(cfg.LogDir + "/pool.csv")
	if err != nil {
		return nil, err
	}
	l.files = []*os.File{dec, pl}
	l.decLog = bufio.NewWriter(dec)
	l.poolLog = bufio.NewWriter(pl)
	fmt.Fprintln(l.decLog, "ts_ms,proto,src_ip,backend,policy,pool_size,remap,outcome,dur_ms")
	fmt.Fprintln(l.poolLog, "ts_ms,event,backend,pool_size,reason")
	return l, nil
}

// Flush writes buffered log lines to disk.
func (l *LB) Flush() {
	l.logMu.Lock()
	defer l.logMu.Unlock()
	l.decLog.Flush()
	l.poolLog.Flush()
}

func (l *LB) Close() {
	l.Flush()
	for _, f := range l.files {
		f.Close()
	}
}

func (l *LB) FlushEvery(d time.Duration) {
	go func() {
		for range time.Tick(d) {
			l.Flush()
		}
	}()
}

func (l *LB) poolEvent(event, id, reason string) {
	up, _ := l.cfg.Pool.Snapshot()
	l.logMu.Lock()
	fmt.Fprintf(l.poolLog, "%d,%s,%s,%d,%s\n", time.Now().UnixMilli(), event, id, len(up), reason)
	l.logMu.Unlock()
}

// AddBackend and RemoveBackend change the pool and record the event.
func (l *LB) AddBackend(id, reason string) bool {
	ok := l.cfg.Pool.Add(id)
	if ok {
		l.poolEvent("add", id, reason)
	}
	return ok
}

func (l *LB) RemoveBackend(id, reason string) bool {
	ok := l.cfg.Pool.Remove(id)
	if ok {
		l.poolEvent("remove", id, reason)
	}
	return ok
}

// choose picks a backend and counts the new connection against it.
func (l *LB) choose(srcIP string) (b *pool.Backend, remap bool, poolSize int) {
	l.pickMu.Lock()
	defer l.pickMu.Unlock()
	up, ver := l.cfg.Pool.Snapshot()
	if len(up) == 0 {
		return nil, false, 0
	}
	b = l.cfg.Policy.Pick(srcIP, up, ver)
	b.Inc()
	prev := l.last[srcIP]
	remap = prev != "" && prev != b.ID
	l.last[srcIP] = b.ID
	return b, remap, len(up)
}

func (l *LB) record(start time.Time, proto, src, backend string, poolSize int, remap bool, outcome string) {
	r := 0
	if remap {
		r = 1
	}
	l.logMu.Lock()
	fmt.Fprintf(l.decLog, "%d,%s,%s,%s,%s,%d,%d,%s,%d\n",
		start.UnixMilli(), proto, src, backend, l.cfg.Policy.Name(), poolSize, r, outcome,
		time.Since(start).Milliseconds())
	l.logMu.Unlock()
}

// ServeTCP proxies TCP connections arriving on addr. kind is "tcp" or "http"
// and only selects which backend port the connection is forwarded to.
func (l *LB) ServeTCP(addr, kind string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	log.Printf("lb: %s listening on %s (policy %s)", kind, addr, l.cfg.Policy.Name())
	for {
		c, err := ln.Accept()
		if err != nil {
			continue
		}
		go l.handleTCP(c, kind)
	}
}

func (l *LB) handleTCP(c net.Conn, kind string) {
	defer c.Close()
	start := time.Now()

	c.SetReadDeadline(start.Add(3 * time.Second))
	br := bufio.NewReaderSize(c, 4096)
	line, err := br.ReadString('\n')
	if err != nil {
		return
	}
	_, src, _, err := proxyline.Parse(line)
	if err != nil {
		return
	}
	c.SetReadDeadline(time.Time{})
	srcIP := src.Addr().String()

	b, remap, poolSize := l.choose(srcIP)
	if b == nil {
		l.record(start, kind, srcIP, "-", 0, false, "no_backend")
		return
	}
	defer b.Dec()

	port := b.TCPPort
	if kind == "http" {
		port = b.HTTPPort
	}
	bc, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", b.Host, port), l.cfg.DialTimeout)
	if err != nil {
		l.record(start, kind, srcIP, b.ID, poolSize, remap, "dial_fail")
		return
	}
	defer bc.Close()

	// Pass the PROXY line on so the backend also sees the real client.
	if _, err := bc.Write([]byte(line)); err != nil {
		l.record(start, kind, srcIP, b.ID, poolSize, remap, "reset")
		return
	}

	go func() { // client -> backend
		io.Copy(bc, br)
		if tc, ok := bc.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	n, err := io.Copy(c, bc) // backend -> client
	outcome := "ok"
	if err != nil || n == 0 {
		outcome = "reset"
	}
	l.record(start, kind, srcIP, b.ID, poolSize, remap, outcome)
}

// ServeUDP forwards each datagram to a backend chosen by the policy and
// relays the reply.
func (l *LB) ServeUDP(addr string) error {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	log.Printf("lb: udp listening on %s (policy %s)", addr, l.cfg.Policy.Name())
	buf := make([]byte, 65536)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			continue
		}
		data := make([]byte, n)
		copy(data, buf[:n])
		go l.handleUDP(pc, from, data)
	}
}

func (l *LB) handleUDP(pc net.PacketConn, from net.Addr, data []byte) {
	start := time.Now()
	_, src, _, _, err := proxyline.ReadUDPProxy(data)
	if err != nil {
		return
	}
	srcIP := src.Addr().String()

	b, remap, poolSize := l.choose(srcIP)
	if b == nil {
		l.record(start, "udp", srcIP, "-", 0, false, "no_backend")
		return
	}
	defer b.Dec()

	raddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", b.Host, b.UDPPort))
	if err != nil {
		l.record(start, "udp", srcIP, b.ID, poolSize, remap, "dial_fail")
		return
	}
	bc, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		l.record(start, "udp", srcIP, b.ID, poolSize, remap, "dial_fail")
		return
	}
	defer bc.Close()

	bc.Write(data)
	bc.SetReadDeadline(time.Now().Add(l.cfg.UDPTimeout))
	reply := make([]byte, 65536)
	n, err := bc.Read(reply)
	if err != nil {
		l.record(start, "udp", srcIP, b.ID, poolSize, remap, "no_reply")
		return
	}
	pc.WriteTo(reply[:n], from)
	l.record(start, "udp", srcIP, b.ID, poolSize, remap, "ok")
}

// ServeAdmin exposes the pool so the churn injector can change it live.
//
//	GET  /pool                 -> {"up": ["backend-1", ...]}
//	POST /pool/add?id=X        -> add backend X to rotation
//	POST /pool/remove?id=X     -> remove backend X from rotation
func (l *LB) ServeAdmin(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/pool", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"up": l.cfg.Pool.IDs()})
	})
	mux.HandleFunc("/pool/add", func(w http.ResponseWriter, r *http.Request) {
		ok := l.AddBackend(r.URL.Query().Get("id"), "churn")
		json.NewEncoder(w).Encode(map[string]any{"changed": ok})
	})
	mux.HandleFunc("/pool/remove", func(w http.ResponseWriter, r *http.Request) {
		ok := l.RemoveBackend(r.URL.Query().Get("id"), "churn")
		json.NewEncoder(w).Encode(map[string]any{"changed": ok})
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	log.Printf("lb: admin listening on %s", addr)
	return http.ListenAndServe(addr, mux)
}
