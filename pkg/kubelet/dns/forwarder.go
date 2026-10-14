// Package dns runs a node-local DNS forwarder. A Pod's network namespace
// cannot reach the resolver in the node's /etc/resolv.conf (Docker's embedded
// DNS on 127.0.0.11 only exists inside the node container), so the kubelet
// points Pods at the node address, where this forwarder relays queries to that
// resolver. It plays the role of Kubernetes' node-local DNS cache, without
// the cache.
package dns

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"time"
)

// Upstream returns the first nameserver of a resolv.conf file, as host:53.
func Upstream(resolvConf string) (string, error) {
	f, err := os.Open(resolvConf)
	if err != nil {
		return "", err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == "nameserver" {
			return net.JoinHostPort(fields[1], "53"), nil
		}
	}
	return "", errors.New("no nameserver in " + resolvConf)
}

// Serve relays UDP DNS queries received on listen to upstream until ctx ends.
func Serve(ctx context.Context, listen, upstream string) error {
	pc, err := net.ListenPacket("udp", listen)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		pc.Close()
	}()
	buf := make([]byte, 65535)
	for {
		n, client, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		query := append([]byte(nil), buf[:n]...)
		go relay(pc, client, query, upstream)
	}
}

func relay(pc net.PacketConn, client net.Addr, query []byte, upstream string) {
	conn, err := net.Dial("udp", upstream)
	if err != nil {
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(query); err != nil {
		return
	}
	resp := make([]byte, 65535)
	n, err := conn.Read(resp)
	if err != nil {
		return
	}
	_, _ = pc.WriteTo(resp[:n], client)
}
