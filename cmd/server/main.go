package main

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"acnlb/internal/proxyline"
)

var (
	serverID = "server-1"
	csvMu    sync.Mutex
	csvFile  *os.File
)

func main() {
	if sid := os.Getenv("SERVER_ID"); sid != "" {
		serverID = sid
	}
	
	os.MkdirAll("/results", 0755)
	f, err := os.OpenFile("/results/server.csv", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		f, _ = os.OpenFile("server.csv", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	}
	csvFile = f
	defer csvFile.Close()
	
	// Write CSV header if empty
	stat, _ := csvFile.Stat()
	if stat != nil && stat.Size() == 0 {
		csvFile.WriteString("ts_unix_ms,proto,src_ip,region,key,req_id\n")
	}

	go serveTCP(":9000")
	go serveUDP(":9001")
	go serveHTTP(":8080")

	select {}
}

func logRequest(proto, srcIP, region, key, reqID string) {
	ts := time.Now().UnixMilli()
	csvMu.Lock()
	fmt.Fprintf(csvFile, "%d,%s,%s,%s,%s,%s\n", ts, proto, srcIP, region, key, reqID)
	csvMu.Unlock()
}

func serveTCP(addr string) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("tcp listen error: %v", err)
	}
	
	pln := &proxyline.Listener{Listener: ln}
	log.Printf("Listening TCP on %s", addr)
	
	for {
		conn, err := pln.Accept()
		if err != nil {
			continue
		}
		go handleTCP(conn)
	}
}

func handleTCP(conn net.Conn) {
	defer conn.Close()
	
	br := bufio.NewReader(conn)
	// Expecting: REQ <req_id> <key> <region> <send_unix_ns>\n
	line, err := br.ReadString('\n')
	if err != nil {
		return
	}
	
	line = strings.TrimSpace(line)
	parts := strings.Split(line, " ")
	if len(parts) >= 4 && parts[0] == "REQ" {
		reqID, key, region := parts[1], parts[2], parts[3]
		srcIP := conn.RemoteAddr().String()
		if host, _, err := net.SplitHostPort(srcIP); err == nil {
			srcIP = host
		}
		
		logRequest("tcp", srcIP, region, key, reqID)
		fmt.Fprintf(conn, "RESP %s %s %s %s\n", reqID, key, serverID, srcIP)
	}
}

func serveUDP(addr string) {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		log.Fatalf("udp listen error: %v", err)
	}
	log.Printf("Listening UDP on %s", addr)
	
	buf := make([]byte, 2048)
	for {
		n, clientAddr, err := pc.ReadFrom(buf)
		if err != nil {
			continue
		}
		
		data := buf[:n]
		_, src, _, payload, err := proxyline.ReadUDPProxy(data)
		if err != nil {
			continue
		}
		
		srcIP := src.Addr().String()
		line := strings.TrimSpace(string(payload))
		parts := strings.Split(line, " ")
		if len(parts) >= 4 && parts[0] == "REQ" {
			reqID, key, region := parts[1], parts[2], parts[3]
			logRequest("udp", srcIP, region, key, reqID)
			
			resp := fmt.Sprintf("RESP %s %s %s %s\n", reqID, key, serverID, srcIP)
			pc.WriteTo([]byte(resp), clientAddr) // Reply to switch (it proxies back)
		}
	}
}

func serveHTTP(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/key/", func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/key/")
		reqID := r.URL.Query().Get("req")
		region := r.URL.Query().Get("region")
		
		srcIP := r.RemoteAddr
		if host, _, err := net.SplitHostPort(srcIP); err == nil {
			srcIP = host
		}
		
		logRequest("http", srcIP, region, key, reqID)
		fmt.Fprintf(w, "RESP %s %s %s %s\n", reqID, key, serverID, srcIP)
	})
	
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("http listen error: %v", err)
	}
	pln := &proxyline.Listener{Listener: ln}
	
	log.Printf("Listening HTTP on %s", addr)
	http.Serve(pln, mux)
}
