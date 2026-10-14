package dns

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUpstreamReadsFirstNameserver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolv.conf")
	_ = os.WriteFile(path, []byte("# generated\nsearch example.com\nnameserver 127.0.0.11\nnameserver 8.8.8.8\noptions ndots:0\n"), 0o644)
	if got, err := Upstream(path); err != nil || got != "127.0.0.11:53" {
		t.Fatalf("%q %v", got, err)
	}
	_ = os.WriteFile(path, []byte("search x\n"), 0o644)
	if _, err := Upstream(path); err == nil {
		t.Fatal("missing nameserver must fail")
	}
}

func TestServeRelaysQueriesToUpstream(t *testing.T) {
	upstream, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := upstream.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = upstream.WriteTo(append([]byte("answer:"), buf[:n]...), addr)
		}
	}()
	probe, _ := net.ListenPacket("udp", "127.0.0.1:0")
	listen := probe.LocalAddr().String()
	probe.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Serve(ctx, listen, upstream.LocalAddr().String()) }()
	time.Sleep(100 * time.Millisecond)
	conn, err := net.Dial("udp", listen)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write([]byte("query")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil || string(buf[:n]) != "answer:query" {
		t.Fatalf("relay: %q %v", buf[:n], err)
	}
}
