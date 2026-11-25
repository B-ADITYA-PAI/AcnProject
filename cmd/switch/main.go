package main

import (
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/netip"
	"sync"
	"time"

	"acnlb/internal/config"
	"acnlb/internal/proxyline"
)

var cfg *config.Config

type regionStats struct {
	conns     uint64
	datagrams uint64
	drops     uint64
	mu        sync.Mutex
}

var statsMap sync.Map // map[string]*regionStats

func getStats(region string) *regionStats {
	v, _ := statsMap.LoadOrStore(region, &regionStats{})
	return v.(*regionStats)
}

func main() {
	var err error
	cfg, err = config.Load("config.yaml")
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	go serveTCP(fmt.Sprintf(":%d", cfg.Server.TCPPort), cfg.Server.TCPPort)
	go serveTCP(fmt.Sprintf(":%d", cfg.Server.HTTPPort), cfg.Server.HTTPPort)
	go serveUDP(fmt.Sprintf(":%d", cfg.Server.UDPPort), cfg.Server.UDPPort)

	log.Printf("Switch running. Client IP: %s, Server IP: %s", cfg.Switch.ClientIP, cfg.Switch.ServerIP)
	
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		for range ticker.C {
			log.Println("--- Region Stats ---")
			statsMap.Range(func(key, value any) bool {
				region := key.(string)
				s := value.(*regionStats)
				s.mu.Lock()
				c, d, dr := s.conns, s.datagrams, s.drops
				s.mu.Unlock()
				log.Printf("  %s: TCP Conns: %d | UDP Datagrams: %d | UDP Drops: %d", region, c, d, dr)
				return true
			})
			log.Println("--------------------")
		}
	}()
	
	select {}
}

func getDelay(region *config.Region) time.Duration {
	base := region.Delay
	jitter := time.Duration(0)
	if region.Jitter > 0 {
		// jitter in [-jitter, +jitter]
		jf := float64(region.Jitter)
		jitter = time.Duration((rand.Float64() * 2 * jf) - jf)
	}
	d := base + jitter
	if d < 0 {
		d = 0
	}
	return d
}

func serveTCP(listenAddr string, upstreamPort int) {
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatalf("tcp listen error on %s: %v", listenAddr, err)
	}
	for {
		clientConn, err := ln.Accept()
		if err != nil {
			continue
		}
		go handleTCP(clientConn, upstreamPort)
	}
}

func handleTCP(clientConn net.Conn, upstreamPort int) {
	defer clientConn.Close()

	srcIPStr, _, _ := net.SplitHostPort(clientConn.RemoteAddr().String())
	srcIP, err := netip.ParseAddr(srcIPStr)
	if err != nil {
		return
	}

	region := cfg.Lookup(srcIP)
	if region == nil {
		log.Printf("unknown src ip %s", srcIPStr)
		return
	}
	
	s := getStats(region.Name)
	s.mu.Lock()
	s.conns++
	s.mu.Unlock()

	serverAddr := fmt.Sprintf("%s:%d", cfg.Switch.ServerIP, upstreamPort)
	serverConn, err := net.Dial("tcp", serverAddr)
	if err != nil {
		return
	}
	defer serverConn.Close()

	srcAP := netip.MustParseAddrPort(clientConn.RemoteAddr().String())
	dstAP := netip.MustParseAddrPort(clientConn.LocalAddr().String())
	proxyLine := proxyline.Format("TCP4", srcAP, dstAP)
	serverConn.Write([]byte(proxyLine))

	var wg sync.WaitGroup
	wg.Add(2)

	// client -> server
	go func() {
		defer wg.Done()
		defer clientConn.Close() // half-close equivalent for simplicity
		buf := make([]byte, 32768)
		first := true
		for {
			n, err := clientConn.Read(buf)
			if n > 0 {
				delay := getDelay(region)
				if first {
					delay += 2 * getDelay(region) // Handshake penalty
					first = false
				}
				if rand.Float64() < region.Loss {
					delay += cfg.Switch.TCPLossPenalty
				}
				time.Sleep(delay)
				serverConn.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
	}()

	// server -> client
	go func() {
		defer wg.Done()
		defer serverConn.Close()
		buf := make([]byte, 32768)
		for {
			n, err := serverConn.Read(buf)
			if n > 0 {
				delay := getDelay(region)
				if rand.Float64() < region.Loss {
					delay += cfg.Switch.TCPLossPenalty
				}
				time.Sleep(delay)
				clientConn.Write(buf[:n])
			}
			if err != nil {
				if err != io.EOF {
					// handle error
				}
				break
			}
		}
	}()

	wg.Wait()
}

type udpSession struct {
	clientAddr net.Addr
	serverConn *net.UDPConn
	lastActive time.Time
}

func serveUDP(listenAddr string, upstreamPort int) {
	clientConn, err := net.ListenPacket("udp", listenAddr)
	if err != nil {
		log.Fatalf("udp listen error: %v", err)
	}

	sessions := make(map[string]*udpSession)
	var mu sync.Mutex

	serverAddrStr := fmt.Sprintf("%s:%d", cfg.Switch.ServerIP, upstreamPort)
	serverUDPAddr, err := net.ResolveUDPAddr("udp", serverAddrStr)
	if err != nil {
		log.Fatalf("resolve server error: %v", err)
	}

	buf := make([]byte, 65536)
	for {
		n, cAddr, err := clientConn.ReadFrom(buf)
		if err != nil {
			continue
		}

		data := make([]byte, n)
		copy(data, buf[:n])

		srcIPStr, _, _ := net.SplitHostPort(cAddr.String())
		srcIP, err := netip.ParseAddr(srcIPStr)
		if err != nil {
			continue
		}

		region := cfg.Lookup(srcIP)
		if region == nil {
			continue
		}

		s := getStats(region.Name)

		if rand.Float64() < region.Loss {
			s.mu.Lock()
			s.drops++
			s.datagrams++
			s.mu.Unlock()
			continue // actual drop
		}

		s.mu.Lock()
		s.datagrams++
		s.mu.Unlock()

		mu.Lock()
		sess, ok := sessions[cAddr.String()]
		if !ok {
			sConn, err := net.DialUDP("udp", nil, serverUDPAddr)
			if err == nil {
				sess = &udpSession{
					clientAddr: cAddr,
					serverConn: sConn,
					lastActive: time.Now(),
				}
				sessions[cAddr.String()] = sess

				// Reader for server -> client
				go func(s *udpSession, r *config.Region) {
					sbuf := make([]byte, 65536)
					for {
						sConn.SetReadDeadline(time.Now().Add(30 * time.Second))
						sn, err := sConn.Read(sbuf)
						if err != nil {
							mu.Lock()
							delete(sessions, s.clientAddr.String())
							mu.Unlock()
							sConn.Close()
							return
						}
						
						sdata := make([]byte, sn)
						copy(sdata, sbuf[:sn])
						
						if rand.Float64() < r.Loss {
							continue // drop
						}
						
						time.AfterFunc(getDelay(r), func() {
							clientConn.WriteTo(sdata, s.clientAddr)
						})
					}
				}(sess, region)
			}
		}
		mu.Unlock()

		if sess != nil {
			sess.lastActive = time.Now()
			time.AfterFunc(getDelay(region), func() {
				srcAP := netip.MustParseAddrPort(cAddr.String())
				// Since we are the switch, the dst is our listen addr. Using dummy.
				dstAP := netip.MustParseAddrPort(fmt.Sprintf("%s:%d", cfg.Switch.ClientIP, upstreamPort))
				proxyLine := proxyline.Format("UDP4", srcAP, dstAP)
				payload := append([]byte(proxyLine), data...)
				sess.serverConn.Write(payload)
			})
		}
	}
}
