package proxyline

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// Format creates a PROXY v1 header line. proto is "TCP4", "TCP6", "UDP4", "UDP6".
func Format(proto string, src, dst netip.AddrPort) string {
	return fmt.Sprintf("PROXY %s %s %s %d %d\r\n", proto, src.Addr().String(), dst.Addr().String(), src.Port(), dst.Port())
}

// Parse parses a PROXY v1 header line.
func Parse(line string) (proto string, src, dst netip.AddrPort, err error) {
	line = strings.TrimSuffix(line, "\r\n")
	parts := strings.Split(line, " ")
	if len(parts) != 6 || parts[0] != "PROXY" {
		return "", netip.AddrPort{}, netip.AddrPort{}, fmt.Errorf("invalid PROXY line")
	}
	proto = parts[1]
	
	srcAddr, err := netip.ParseAddr(parts[2])
	if err != nil {
		return "", netip.AddrPort{}, netip.AddrPort{}, fmt.Errorf("invalid src IP: %v", err)
	}
	dstAddr, err := netip.ParseAddr(parts[3])
	if err != nil {
		return "", netip.AddrPort{}, netip.AddrPort{}, fmt.Errorf("invalid dst IP: %v", err)
	}
	
	var sport, dport uint16
	if _, err := fmt.Sscanf(parts[4], "%d", &sport); err != nil {
		return "", netip.AddrPort{}, netip.AddrPort{}, fmt.Errorf("invalid src port: %v", err)
	}
	if _, err := fmt.Sscanf(parts[5], "%d", &dport); err != nil {
		return "", netip.AddrPort{}, netip.AddrPort{}, fmt.Errorf("invalid dst port: %v", err)
	}
	
	src = netip.AddrPortFrom(srcAddr, sport)
	dst = netip.AddrPortFrom(dstAddr, dport)
	return proto, src, dst, nil
}

// Listener wraps a net.Listener to read the PROXY line on Accept.
type Listener struct {
	net.Listener
}

func (l *Listener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to read PROXY line: %v", err)
	}
	
	_, src, _, err := Parse(line)
	if err != nil {
		conn.Close()
		return nil, err
	}
	
	return &proxyConn{
		Conn: conn,
		r:    br,
		src:  net.UDPAddrFromAddrPort(src), // Just using UDPAddr to hold IP/Port easily for net.Addr
	}, nil
}

type proxyConn struct {
	net.Conn
	r   *bufio.Reader
	src net.Addr
}

func (c *proxyConn) Read(b []byte) (int, error) {
	return c.r.Read(b)
}

func (c *proxyConn) RemoteAddr() net.Addr {
	return c.src
}

func ReadUDPProxy(data []byte) (proto string, src, dst netip.AddrPort, payload []byte, err error) {
	idx := bytes.Index(data, []byte("\r\n"))
	if idx == -1 {
		return "", netip.AddrPort{}, netip.AddrPort{}, nil, fmt.Errorf("no PROXY line found")
	}
	line := string(data[:idx+2])
	proto, src, dst, err = Parse(line)
	if err != nil {
		return "", netip.AddrPort{}, netip.AddrPort{}, nil, err
	}
	return proto, src, dst, data[idx+2:], nil
}
