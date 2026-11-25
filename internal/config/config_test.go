package config

import (
	"net/netip"
	"testing"
	"time"
)

func TestConfigLookup(t *testing.T) {
	c := &Config{
		Regions: []Region{
			{Name: "ap-south", CIDR: "10.10.1.0/24", prefix: netip.MustParsePrefix("10.10.1.0/24"), Delay: 2 * time.Millisecond},
		},
	}
	
	if r := c.Lookup(netip.MustParseAddr("10.10.1.5")); r == nil || r.Name != "ap-south" {
		t.Fatalf("expected to find ap-south, got %v", r)
	}
	
	if r := c.Lookup(netip.MustParseAddr("10.10.2.5")); r != nil {
		t.Fatalf("expected nil for unknown ip, got %v", r)
	}
}
