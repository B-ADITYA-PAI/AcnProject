// Command lb runs the L4 load balancer (the device under test).
//
// Configuration is by environment variable (see docker-compose.lb.yml):
//
//	POLICY        rr | hash | least | consistent        (default rr)
//	POOL_SIZE     backends initially in rotation        (default 5)
//	BACKEND_MAX   backends that exist (backend-1..N)    (default 10)
//	BACKEND_HOST  host the backends listen on           (default 10.20.0.30)
//	BACKEND_BASE  base port, see internal/pool          (default 10000)
//	TCP_PORT / UDP_PORT / HTTP_PORT / ADMIN_PORT        (9000 / 9001 / 8080 / 9100)
//	LOG_DIR       where lb.csv and pool.csv are written (default /results)
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"acnlb/internal/envx"
	"acnlb/internal/lb"
	"acnlb/internal/policy"
	"acnlb/internal/pool"
)

func main() {
	pol, err := policy.New(envx.Str("POLICY", "rr"))
	if err != nil {
		log.Fatal(err)
	}

	maxN := envx.Int("BACKEND_MAX", 10)
	poolSize := envx.Int("POOL_SIZE", 5)
	if poolSize > maxN {
		log.Fatalf("POOL_SIZE %d exceeds BACKEND_MAX %d", poolSize, maxN)
	}
	host := envx.Str("BACKEND_HOST", "10.20.0.30")
	base := envx.Int("BACKEND_BASE", 10000)

	p := pool.New()
	for i := 1; i <= maxN; i++ {
		p.Register(pool.NewBackend(i, host, base))
	}

	l, err := lb.New(lb.Config{Policy: pol, Pool: p, LogDir: envx.Str("LOG_DIR", "/results")})
	if err != nil {
		log.Fatal(err)
	}
	l.FlushEvery(500 * time.Millisecond)

	for i := 1; i <= poolSize; i++ {
		l.AddBackend(fmt.Sprintf("backend-%d", i), "init")
	}
	log.Printf("lb: policy=%s pool=%v", pol.Name(), p.IDs())

	go func() { log.Fatal(l.ServeTCP(fmt.Sprintf(":%d", envx.Int("TCP_PORT", 9000)), "tcp")) }()
	go func() { log.Fatal(l.ServeTCP(fmt.Sprintf(":%d", envx.Int("HTTP_PORT", 8080)), "http")) }()
	go func() { log.Fatal(l.ServeUDP(fmt.Sprintf(":%d", envx.Int("UDP_PORT", 9001)))) }()
	go func() { log.Fatal(l.ServeAdmin(fmt.Sprintf(":%d", envx.Int("ADMIN_PORT", 9100)))) }()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	l.Close()
}
