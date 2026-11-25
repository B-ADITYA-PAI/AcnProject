// Command churn is the churn injector. Every CHURN_INTERVAL it alternately
// removes a random backend from the pool and brings it back, so the pool
// oscillates between N and N-1 while traffic is live.
//
// A removal is done like a real failure: the backend is stopped first (its open
// connections are reset), and the load balancer learns about it DETECT_DELAY
// later, as it would after a failed health check. Requests sent to the dead
// backend in that gap are the client-visible failures.
//
//	CHURN_INTERVAL   time between events; 0 disables churn   (default 0)
//	CHURN_WARMUP     wait before the first event             (default 5s)
//	DETECT_DELAY     stop -> LB removal delay                (default 500ms)
//	MIN_POOL         never go below this many backends       (default 2)
//	LB_ADMIN         load balancer admin URL    (default http://10.20.0.10:9100)
//	BACKENDS_ADMIN   backend admin URL          (default http://10.20.0.30:9200)
//	LOG_DIR          churn.csv goes here                      (default /results)
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"time"

	"acnlb/internal/envx"
)

var client = &http.Client{Timeout: 3 * time.Second}

func post(url string) error {
	resp, err := client.Post(url, "text/plain", nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return nil
}

func upIDs(lb string) ([]string, error) {
	resp, err := client.Get(lb + "/pool")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var v struct{ Up []string }
	return v.Up, json.NewDecoder(resp.Body).Decode(&v)
}

func main() {
	interval := envx.Duration("CHURN_INTERVAL", 0)
	if interval <= 0 {
		log.Println("churn: disabled (CHURN_INTERVAL=0)")
		select {}
	}
	warmup := envx.Duration("CHURN_WARMUP", 5*time.Second)
	detect := envx.Duration("DETECT_DELAY", 500*time.Millisecond)
	minPool := envx.Int("MIN_POOL", 2)
	lb := envx.Str("LB_ADMIN", "http://10.20.0.10:9100")
	be := envx.Str("BACKENDS_ADMIN", "http://10.20.0.30:9200")

	os.MkdirAll(envx.Str("LOG_DIR", "/results"), 0o755)
	f, err := os.Create(envx.Str("LOG_DIR", "/results") + "/churn.csv")
	if err != nil {
		log.Fatal(err)
	}
	w := bufio.NewWriter(f)
	fmt.Fprintln(w, "ts_ms,event,backend")
	emit := func(event, id string) {
		fmt.Fprintf(w, "%d,%s,%s\n", time.Now().UnixMilli(), event, id)
		w.Flush()
		log.Printf("churn: %s %s", event, id)
	}

	rng := rand.New(rand.NewSource(int64(envx.Int("SEED", 42))))
	time.Sleep(warmup)

	removed := ""
	for {
		if removed == "" {
			up, err := upIDs(lb)
			if err != nil || len(up) <= minPool {
				log.Printf("churn: skipping removal (up=%v err=%v)", up, err)
			} else {
				id := up[rng.Intn(len(up))]
				emit("stop", id)
				if err := post(be + "/stop?id=" + id); err != nil {
					log.Println("churn:", err)
				} else {
					time.Sleep(detect)
					emit("remove", id)
					post(lb + "/pool/remove?id=" + id)
					removed = id
				}
			}
		} else {
			emit("start", removed)
			if err := post(be + "/start?id=" + removed); err != nil {
				log.Println("churn:", err)
			} else {
				time.Sleep(100 * time.Millisecond)
				emit("add", removed)
				post(lb + "/pool/add?id=" + removed)
				removed = ""
			}
		}
		time.Sleep(interval)
	}
}
