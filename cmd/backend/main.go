// Command backend runs a pool of lightweight echo backends in one process.
//
// Backend i (1..BACKEND_MAX) listens on BACKEND_BASE+10*i (TCP), +1 (UDP) and
// +2 (HTTP). Each request takes SERVICE_TIME to answer, so connections stay
// open long enough for churn to catch some in flight. An admin API lets the
// churn injector stop and start backends:
//
//	GET  /load            -> active connections per backend (the load signal)
//	POST /stop?id=X       -> close X's listeners and RESET its open connections
//	POST /start?id=X      -> start X again
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"acnlb/internal/envx"
	"acnlb/internal/proxyline"
)

type logger struct {
	mu sync.Mutex
	w  *bufio.Writer
}

func (l *logger) write(id, proto, src, region, key, req string) {
	l.mu.Lock()
	fmt.Fprintf(l.w, "%d,%s,%s,%s,%s,%s,%s\n", time.Now().UnixMilli(), id, proto, src, region, key, req)
	l.mu.Unlock()
}

type backend struct {
	id                         string
	tcpPort, udpPort, httpPort int
	svc                        time.Duration
	lg                         *logger

	mu      sync.Mutex
	running bool
	closers []io.Closer
	conns   map[net.Conn]struct{}
	active  atomic.Int64
}

func (b *backend) Start() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running {
		return nil
	}
	tcp, err := net.Listen("tcp", fmt.Sprintf(":%d", b.tcpPort))
	if err != nil {
		return err
	}
	http_, err := net.Listen("tcp", fmt.Sprintf(":%d", b.httpPort))
	if err != nil {
		tcp.Close()
		return err
	}
	udp, err := net.ListenPacket("udp", fmt.Sprintf(":%d", b.udpPort))
	if err != nil {
		tcp.Close()
		http_.Close()
		return err
	}
	b.closers = []io.Closer{tcp, http_, udp}
	b.conns = make(map[net.Conn]struct{})
	b.running = true
	go b.acceptLoop(tcp, "tcp")
	go b.acceptLoop(http_, "http")
	go b.udpLoop(udp)
	return nil
}

// Stop closes the listeners and resets every open connection, like a crashed
// backend would.
func (b *backend) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.running {
		return
	}
	b.running = false
	for _, c := range b.closers {
		c.Close()
	}
	for c := range b.conns {
		if tc, ok := c.(*net.TCPConn); ok {
			tc.SetLinger(0) // close sends RST instead of FIN
		}
		c.Close()
	}
	b.conns = nil
}

func (b *backend) track(c net.Conn) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.running {
		return false
	}
	b.conns[c] = struct{}{}
	return true
}

func (b *backend) untrack(c net.Conn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conns != nil {
		delete(b.conns, c)
	}
}

func (b *backend) acceptLoop(ln net.Listener, kind string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return // listener closed by Stop
		}
		if !b.track(c) {
			c.Close()
			continue
		}
		go b.serve(c, kind)
	}
}

func (b *backend) serve(c net.Conn, kind string) {
	b.active.Add(1)
	defer b.active.Add(-1)
	defer b.untrack(c)
	defer c.Close()

	c.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(c)
	pl, err := br.ReadString('\n')
	if err != nil {
		return
	}
	_, src, _, err := proxyline.Parse(pl)
	if err != nil {
		return
	}
	srcIP := src.Addr().String()

	req, err := br.ReadString('\n')
	if err != nil {
		return
	}
	req = strings.TrimSpace(req)

	if kind == "http" {
		// "GET /key/<key>?req=<id>&region=<r> HTTP/1.1" then headers.
		parts := strings.Fields(req)
		if len(parts) < 2 {
			return
		}
		for { // drain headers
			h, err := br.ReadString('\n')
			if err != nil || strings.TrimSpace(h) == "" {
				break
			}
		}
		u, err := url.Parse(parts[1])
		if err != nil {
			return
		}
		key := strings.TrimPrefix(u.Path, "/key/")
		reqID, region := u.Query().Get("req"), u.Query().Get("region")
		time.Sleep(b.svc)
		b.lg.write(b.id, "http", srcIP, region, key, reqID)
		body := fmt.Sprintf("RESP %s %s %s %s\n", reqID, key, b.id, srcIP)
		fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
		return
	}

	// "REQ <req_id> <key> <region> <send_ns>"
	f := strings.Fields(req)
	if len(f) < 4 || f[0] != "REQ" {
		return
	}
	time.Sleep(b.svc)
	b.lg.write(b.id, "tcp", srcIP, f[3], f[2], f[1])
	fmt.Fprintf(c, "RESP %s %s %s %s\n", f[1], f[2], b.id, srcIP)
}

func (b *backend) udpLoop(pc net.PacketConn) {
	buf := make([]byte, 2048)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		data := make([]byte, n)
		copy(data, buf[:n])
		go func() {
			_, src, _, payload, err := proxyline.ReadUDPProxy(data)
			if err != nil {
				return
			}
			f := strings.Fields(string(payload))
			if len(f) < 4 || f[0] != "REQ" {
				return
			}
			b.active.Add(1)
			time.Sleep(b.svc)
			b.active.Add(-1)
			srcIP := src.Addr().String()
			b.lg.write(b.id, "udp", srcIP, f[3], f[2], f[1])
			pc.WriteTo([]byte(fmt.Sprintf("RESP %s %s %s %s\n", f[1], f[2], b.id, srcIP)), from)
		}()
	}
}

func main() {
	maxN := envx.Int("BACKEND_MAX", 10)
	base := envx.Int("BACKEND_BASE", 10000)
	svc := envx.Duration("SERVICE_TIME", 30*time.Millisecond)

	dir := envx.Str("LOG_DIR", "/results")
	os.MkdirAll(dir, 0o755)
	f, err := os.Create(dir + "/backend.csv")
	if err != nil {
		log.Fatal(err)
	}
	lg := &logger{w: bufio.NewWriter(f)}
	fmt.Fprintln(lg.w, "ts_ms,backend,proto,src_ip,region,key,req_id")
	go func() {
		for range time.Tick(500 * time.Millisecond) {
			lg.mu.Lock()
			lg.w.Flush()
			lg.mu.Unlock()
		}
	}()

	backends := map[string]*backend{}
	for i := 1; i <= maxN; i++ {
		p := base + 10*i
		b := &backend{id: fmt.Sprintf("backend-%d", i), tcpPort: p, udpPort: p + 1, httpPort: p + 2, svc: svc, lg: lg}
		if err := b.Start(); err != nil {
			log.Fatalf("%s: %v", b.id, err)
		}
		backends[b.id] = b
	}
	log.Printf("backend: %d backends up, service time %s", maxN, svc)

	mux := http.NewServeMux()
	mux.HandleFunc("/load", func(w http.ResponseWriter, r *http.Request) {
		out := map[string]int64{}
		for id, b := range backends {
			out[id] = b.active.Load()
		}
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/stop", func(w http.ResponseWriter, r *http.Request) {
		b, ok := backends[r.URL.Query().Get("id")]
		if !ok {
			http.Error(w, "unknown backend", 404)
			return
		}
		b.Stop()
		io.WriteString(w, "stopped\n")
	})
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		b, ok := backends[r.URL.Query().Get("id")]
		if !ok {
			http.Error(w, "unknown backend", 404)
			return
		}
		if err := b.Start(); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		io.WriteString(w, "started\n")
	})
	go func() {
		log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", envx.Int("ADMIN_PORT", 9200)), mux))
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	lg.mu.Lock()
	lg.w.Flush()
	lg.mu.Unlock()
}
