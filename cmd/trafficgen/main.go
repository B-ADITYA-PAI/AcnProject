package main

import (
	"bufio"
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"acnlb/internal/config"
	"acnlb/internal/zipf"
)

type Record struct {
	ts      int64
	region  string
	clientIP string
	proto   string
	reqID   string
	key     int
	latency int64
	outcome string
	seenIP  string
}

func main() {
	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	os.MkdirAll("/results", 0755)
	f, err := os.Create("/results/gen.csv")
	if err != nil {
		f, _ = os.Create("gen.csv")
	}
	defer f.Close()
	fmt.Fprintln(f, "ts_unix_ms,region,client_ip,proto,req_id,key,latency_ms,outcome,seen_ip")

	records := make(chan Record, 10000)
	var allRecords []Record
	
	var readerWg sync.WaitGroup
	readerWg.Add(1)
	go func() {
		defer readerWg.Done()
		for r := range records {
			allRecords = append(allRecords, r)
			fmt.Fprintf(f, "%d,%s,%s,%s,%s,%d,%d,%s,%s\n",
				r.ts, r.region, r.clientIP, r.proto, r.reqID, r.key, r.latency, r.outcome, r.seenIP)
		}
	}()

	var wg sync.WaitGroup
	var inflightWg sync.WaitGroup
	
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Traffic.Duration)
	defer cancel()

	for _, region := range cfg.Regions {
		wg.Add(1)
		go runRegion(ctx, cfg, region, records, &wg, &inflightWg)
	}

	wg.Wait()
	inflightWg.Wait()
	close(records)
	readerWg.Wait()

	printSummary(cfg, allRecords)
}

func runRegion(ctx context.Context, cfg *config.Config, region config.Region, records chan<- Record, wg *sync.WaitGroup, inflightWg *sync.WaitGroup) {

	defer wg.Done()

	r := rand.New(rand.NewSource(cfg.Traffic.Seed + int64(len(region.Name))))
	z := zipf.New(cfg.Traffic.Keys, cfg.Traffic.ZipfAlpha)
	
	// Create alias list (we won't bind them if they don't exist locally, just use as src in logic or bind if possible)
	// We'll skip actually binding to the local interface unless it exists, but we can set LocalAddr and hope for the best.
	// For testing on mac without full net setup, we might fail to bind. 
	// To make this robust, we'll try to bind, if it fails, we fall back to nil (default IP).
	
	rate := cfg.Traffic.RatePerRegion
	var reqNum uint64
	
	for {
		gap := rand.ExpFloat64() / rate
		
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(gap * float64(time.Second))):
		}
		
		reqNum++
		key := z.Sample(r)
		
		h := fnv.New32a()
		h.Write([]byte(fmt.Sprintf("%d", key)))
		aliasIdx := int(h.Sum32()) % region.Clients
		
		// 10.10.x.y where x=region_id, y=aliasIdx+1
		// For simplicity, we just format the IP based on CIDR
		base := region.Prefix().Addr().As4()
		clientIP := fmt.Sprintf("%d.%d.%d.%d", base[0], base[1], base[2], aliasIdx+1)
		
		p := r.Float64()
		proto := "tcp"
		if p > cfg.Traffic.Mix["tcp"] {
			if p > cfg.Traffic.Mix["tcp"] + cfg.Traffic.Mix["udp"] {
				proto = "http"
			} else {
				proto = "udp"
			}
		}

		inflightWg.Add(1)
		go sendRequest(cfg, region, clientIP, proto, fmt.Sprintf("%s-%d", region.Name, reqNum), key, records, inflightWg)
	}
}

func sendRequest(cfg *config.Config, region config.Region, clientIP string, proto, reqID string, key int, records chan<- Record, inflightWg *sync.WaitGroup) {
	defer inflightWg.Done()
	start := time.Now()
	rec := Record{
		ts:       start.UnixMilli(),
		region:   region.Name,
		clientIP: clientIP,
		proto:    proto,
		reqID:    reqID,
		key:      key,
		outcome:  "timeout",
	}
	defer func() {
		rec.latency = time.Since(start).Milliseconds()
		records <- rec
	}()

	clientAddr := &net.TCPAddr{IP: net.ParseIP(clientIP), Port: 0}
	dialer := &net.Dialer{
		Timeout:   cfg.Traffic.Timeout,
		LocalAddr: clientAddr,
	}
	
	switch proto {
	case "tcp":
		conn, err := dialer.Dial("tcp", fmt.Sprintf("%s:%d", cfg.Switch.ClientIP, cfg.Server.TCPPort))
		if err != nil {
			// Fallback if clientIP binding fails locally (e.g. dev machine)
			dialer.LocalAddr = nil
			conn, err = dialer.Dial("tcp", fmt.Sprintf("%s:%d", cfg.Switch.ClientIP, cfg.Server.TCPPort))
			if err != nil {
				rec.outcome = "error"
				return
			}
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(cfg.Traffic.Timeout))
		fmt.Fprintf(conn, "REQ %s %d %s %d\n", reqID, key, region.Name, start.UnixNano())
		br := bufio.NewReader(conn)
		resp, err := br.ReadString('\n')
		if err == nil && strings.HasPrefix(resp, "RESP") {
			rec.outcome = "ok"
			parts := strings.Split(strings.TrimSpace(resp), " ")
			if len(parts) >= 5 {
				rec.seenIP = parts[4]
			}
		}
	case "udp":
		cAddr := &net.UDPAddr{IP: net.ParseIP(clientIP), Port: 0}
		sAddr, _ := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", cfg.Switch.ClientIP, cfg.Server.UDPPort))
		conn, err := net.DialUDP("udp", cAddr, sAddr)
		if err != nil {
			conn, err = net.DialUDP("udp", nil, sAddr)
			if err != nil {
				rec.outcome = "error"
				return
			}
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(cfg.Traffic.Timeout))
		fmt.Fprintf(conn, "REQ %s %d %s %d\n", reqID, key, region.Name, start.UnixNano())
		buf := make([]byte, 1024)
		n, err := conn.Read(buf)
		if err == nil {
			resp := string(buf[:n])
			if strings.HasPrefix(resp, "RESP") {
				rec.outcome = "ok"
				parts := strings.Split(strings.TrimSpace(resp), " ")
				if len(parts) >= 5 {
					rec.seenIP = parts[4]
				}
			}
		}
	case "http":
		client := &http.Client{
			Transport: &http.Transport{DialContext: dialer.DialContext, DisableKeepAlives: true},
			Timeout:   cfg.Traffic.Timeout,
		}
		url := fmt.Sprintf("http://%s:%d/key/%d?req=%s&region=%s", cfg.Switch.ClientIP, cfg.Server.HTTPPort, key, reqID, region.Name)
		resp, err := client.Get(url)
		if err != nil {
			// fallback
			client.Transport = &http.Transport{DisableKeepAlives: true}
			resp, err = client.Get(url)
			if err != nil {
				rec.outcome = "error"
				return
			}
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		sbody := string(body)
		if strings.HasPrefix(sbody, "RESP") {
			rec.outcome = "ok"
			parts := strings.Split(strings.TrimSpace(sbody), " ")
			if len(parts) >= 5 {
				rec.seenIP = parts[4]
			}
		}
	}
}

func printSummary(cfg *config.Config, records []Record) {
	fmt.Println("\n=== Traffic Summary ===")
	
	// by region and proto
	type stat struct {
		sent, ok, timeout int
		latencies         []int64
	}
	stats := make(map[string]map[string]*stat)
	keyCounts := make(map[int]int)
	
	for _, r := range records {
		if stats[r.region] == nil {
			stats[r.region] = make(map[string]*stat)
		}
		if stats[r.region][r.proto] == nil {
			stats[r.region][r.proto] = &stat{}
		}
		
		s := stats[r.region][r.proto]
		s.sent++
		if r.outcome == "ok" {
			s.ok++
			s.latencies = append(s.latencies, r.latency)
		} else {
			s.timeout++
		}
		keyCounts[r.key]++
	}
	
	for _, reg := range cfg.Regions {
		fmt.Printf("Region: %s\n", reg.Name)
		for _, proto := range []string{"tcp", "udp", "http"} {
			s := stats[reg.Name][proto]
			if s == nil || s.sent == 0 {
				continue
			}
			sort.Slice(s.latencies, func(i, j int) bool { return s.latencies[i] < s.latencies[j] })
			
			p50, p95, p99 := int64(0), int64(0), int64(0)
			if len(s.latencies) > 0 {
				p50 = s.latencies[int(0.50*float64(len(s.latencies)))]
				p95 = s.latencies[int(0.95*float64(len(s.latencies)))]
				p99 = s.latencies[int(0.99*float64(len(s.latencies)))]
			}
			
			fmt.Printf("  %-4s: Sent %4d | OK %4d | TO %4d | p50 %4d ms | p95 %4d ms | p99 %4d ms\n",
				proto, s.sent, s.ok, s.timeout, p50, p95, p99)
		}
	}
	
	type kc struct{ key, count int }
	var kcs []kc
	for k, c := range keyCounts {
		kcs = append(kcs, kc{k, c})
	}
	sort.Slice(kcs, func(i, j int) bool { return kcs[i].count > kcs[j].count })
	
	fmt.Printf("\nTop 5 Keys (Total reqs: %d):\n", len(records))
	for i := 0; i < 5 && i < len(kcs); i++ {
		share := float64(kcs[i].count) / float64(len(records)) * 100
		fmt.Printf("  Key %4d: %4d reqs (%.1f%%)\n", kcs[i].key, kcs[i].count, share)
	}
	fmt.Println("=======================")
}
