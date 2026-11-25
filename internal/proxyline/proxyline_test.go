package proxyline

import (
	"net/netip"
	"testing"
)

func TestFormatAndParse(t *testing.T) {
	src := netip.MustParseAddrPort("10.10.3.7:51234")
	dst := netip.MustParseAddrPort("10.20.0.10:9000")
	line := Format("TCP4", src, dst)
	
	expected := "PROXY TCP4 10.10.3.7 10.20.0.10 51234 9000\r\n"
	if line != expected {
		t.Fatalf("expected %q, got %q", expected, line)
	}
	
	proto, psrc, pdst, err := Parse(line)
	if err != nil {
		t.Fatal(err)
	}
	if proto != "TCP4" || psrc != src || pdst != dst {
		t.Fatalf("parse failed: %s %v %v", proto, psrc, pdst)
	}
}
