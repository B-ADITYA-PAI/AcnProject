// Command quickgen is a Docker-free traffic generator for local runs.
//
// It sends Zipf-distributed requests straight to the load balancer, putting
// the PROXY line (normally added by the switch) in front of each request, so
// the LB sees the same source IPs as in the full stack. There is no regional
// delay or loss, so use it for fairness / churn / affinity experiments; use
// the full Docker stack (with the switch) for latency results.
// It writes gen.csv in the same format as cmd/trafficgen.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"math/rand"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"acnlb/internal/proxyline"
	"acnlb/internal/zipf"
)

var regions = []string{"ap-south", "ap-southeast", "eu-west", "us-east"}

func main() {
	host := flag.String("host", "127.0.0.1", "load balancer host")
	tcpPort := flag.Int("tcp", 9000, "LB TCP port")
	udpPort := flag.Int("udp", 9001, "LB UDP port")
	httpPort := flag.Int("http", 8080, "LB HTTP port")
	rate := flag.Float64("rate", 50, "requests per second per region")
	dur := flag.Duration("duration", 30*time.Second, "how long to send")
	keys := flag.Int("keys", 1000, "number of distinct keys")
	alpha := flag.Float64("alpha", 1.0, "Zipf skew")
	clients := flag.Int("clients", 20, "client IPs per region")
	seed := flag.Int64("seed", 42, "random seed")
	timeout := flag.Duration("timeout", 2*time.Second, "per-request timeout")
	out := flag.String("out", "gen.csv", "output CSV")
	flag.Parse()

	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	fmt.Fprintln(w, "ts_unix_ms,region,client_ip,proto,req_id,key,latency_ms,outcome,seen_ip")
	var wmu sync.Mutex

	var wg, inflight sync.WaitGroup
	deadline := time.Now().Add(*dur)
	for ri, region := range regions {
		wg.Add(1)
		go func(ri int, region string) {
			defer wg.Done()
			r := rand.New(rand.NewSource(*seed + int64(len(region))))
			z := zipf.New(*keys, *alpha)
			var n uint64
			for time.Now().Before(deadline) {
				time.Sleep(time.Duration(r.ExpFloat64() / *rate * float64(time.Second)))
				n++
				key := z.Sample(r)
				h := fnv.New32a()
				fmt.Fprintf(h, "%d", key)
				idx := int(h.Sum32()) % *clients
				ip := fmt.Sprintf("10.10.%d.%d", ri+1, idx+1)
				p := r.Float64()
				proto := "tcp"
				if p > 0.5 {
					proto = "udp"
					if p > 0.8 {
						proto = "http"
					}
				}
				inflight.Add(1)
				go func(id string, key int) {
					defer inflight.Done()
					start := time.Now()
					outcome, seen := send(*host, *tcpPort, *udpPort, *httpPort, proto, ip, id, key, region, *timeout)
					wmu.Lock()
					fmt.Fprintf(w, "%d,%s,%s,%s,%s,%d,%d,%s,%s\n", start.UnixMilli(), region, ip, proto, id, key,
						time.Since(start).Milliseconds(), outcome, seen)
					wmu.Unlock()
				}(fmt.Sprintf("%s-%d", region, n), key)
			}
		}(ri, region)
	}
	wg.Wait()
	inflight.Wait()
	w.Flush()
}

func proxy(kind, ip string, dport int) string {
	src := netip.AddrPortFrom(netip.MustParseAddr(ip), 40000)
	dst := netip.AddrPortFrom(netip.MustParseAddr("10.10.0.2"), uint16(dport))
	return proxyline.Format(kind, src, dst)
}

func send(host string, tcpPort, udpPort, httpPort int, proto, ip, id string, key int, region string, to time.Duration) (string, string) {
	seen := func(resp string) (string, string) {
		if !strings.HasPrefix(resp, "RESP") {
			return "timeout", ""
		}
		f := strings.Fields(resp)
		if len(f) >= 5 {
			return "ok", f[4]
		}
		return "ok", ""
	}
	switch proto {
	case "udp":
		c, err := net.Dial("udp", fmt.Sprintf("%s:%d", host, udpPort))
		if err != nil {
			return "error", ""
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(to))
		fmt.Fprintf(c, "%sREQ %s %d %s %d\n", proxy("UDP4", ip, udpPort), id, key, region, time.Now().UnixNano())
		buf := make([]byte, 1024)
		n, err := c.Read(buf)
		if err != nil {
			return "timeout", ""
		}
		return seen(string(buf[:n]))
	case "http":
		c, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, httpPort), to)
		if err != nil {
			return "error", ""
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(to))
		fmt.Fprintf(c, "%sGET /key/%d?req=%s&region=%s HTTP/1.1\r\nHost: lb\r\nConnection: close\r\n\r\n",
			proxy("TCP4", ip, httpPort), key, id, region)
		b, _ := io.ReadAll(c)
		i := strings.Index(string(b), "\r\n\r\n")
		if i < 0 {
			return "timeout", ""
		}
		return seen(string(b[i+4:]))
	default:
		c, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, tcpPort), to)
		if err != nil {
			return "error", ""
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(to))
		fmt.Fprintf(c, "%sREQ %s %d %s %d\n", proxy("TCP4", ip, tcpPort), id, key, region, time.Now().UnixNano())
		resp, err := bufio.NewReader(c).ReadString('\n')
		if err != nil {
			return "timeout", ""
		}
		return seen(resp)
	}
}

