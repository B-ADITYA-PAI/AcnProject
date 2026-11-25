package pool

import "testing"

func TestAddRemove(t *testing.T) {
	p := New()
	for i := 1; i <= 3; i++ {
		p.Register(NewBackend(i, "h", 10000))
	}
	if p.Add("backend-9") {
		t.Fatal("unknown backend must not be added")
	}
	p.Add("backend-3")
	p.Add("backend-1")
	if p.Add("backend-1") {
		t.Fatal("double add must be a no-op")
	}
	up, v1 := p.Snapshot()
	if len(up) != 2 || up[0].ID != "backend-1" || up[1].ID != "backend-3" {
		t.Fatalf("want sorted [1 3], got %v", p.IDs())
	}
	if !p.Remove("backend-1") || p.Remove("backend-1") {
		t.Fatal("remove should succeed once")
	}
	_, v2 := p.Snapshot()
	if v2 == v1 {
		t.Fatal("version must change on removal")
	}
}

func TestPorts(t *testing.T) {
	b := NewBackend(2, "h", 10000)
	if b.TCPPort != 10020 || b.UDPPort != 10021 || b.HTTPPort != 10022 {
		t.Fatalf("unexpected ports %+v", b)
	}
}
