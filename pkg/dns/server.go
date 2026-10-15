package dns

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

// Server answers queries for the zone and forwards the rest upstream.
type Server struct {
	Zone *Zone
	// Forward sends a query to the upstream resolver and returns its
	// answer; nil uses UDP to Upstream.
	Forward func(query []byte) ([]byte, error)
	// Upstream is the resolver for names outside the zone, host:port.
	Upstream string
}

const (
	maxUDP = 512 // without EDNS, as in RFC 1035
	// maxInFlight bounds the UDP queries handled at once.
	maxInFlight = 512
)

// Handle answers one query; maxSize bounds a UDP answer (0 for TCP). It
// returns nil for messages that cannot be answered at all.
func (s *Server) Handle(query []byte, maxSize int) []byte {
	if len(query) < 12 {
		return nil
	}
	// One question per query, as every resolver sends; anything else gets
	// a bare error before parsing.
	var m *Message
	err := errors.New("dns: not exactly one question")
	if binary.BigEndian.Uint16(query[4:]) == 1 {
		m, err = Parse(query)
	}
	if err != nil {
		reply := &Message{ID: binary.BigEndian.Uint16(query), Response: true, Rcode: RcodeFormatError}
		return reply.Pack()
	}
	if m.Response {
		return nil
	}
	reply := &Message{ID: m.ID, Response: true, Opcode: m.Opcode, RecursionDesired: m.RecursionDesired,
		RecursionAvailable: true, Questions: m.Questions}
	if m.Opcode != 0 {
		reply.Rcode = RcodeNotImplemented
		return reply.Pack()
	}
	q := m.Questions[0]
	answers, rcode, inZone, external := s.Zone.Resolve(q)
	if !inZone {
		resp, err := s.forward(query)
		if err != nil {
			reply.Rcode = RcodeServerFailure
			return reply.Pack()
		}
		return resp
	}
	if external != "" && q.Type != TypeCNAME {
		// Like CoreDNS, follow an ExternalName's CNAME upstream so clients
		// get the addresses in one answer.
		answers = append(answers, s.chase(external, q.Type)...)
	}
	reply.Authoritative, reply.Rcode, reply.Answers = true, rcode, answers
	if len(answers) == 0 {
		reply.Authority = []RR{SOA(s.Zone.Domain+".", s.Zone.TTL)}
	}
	if maxSize > 0 {
		limit := maxUDP
		for _, rr := range m.Additional {
			if rr.Type == TypeOPT && int(rr.Class) > limit { // EDNS: the class is the UDP payload size
				limit = min(int(rr.Class), 4096)
			}
		}
		if packed := reply.Pack(); len(packed) > limit {
			reply.Truncated, reply.Answers, reply.Authority = true, nil, nil // the client retries over TCP
		}
	}
	return reply.Pack()
}

// chase resolves target (in the zone, or upstream) and returns its records
// of type qtype.
func (s *Server) chase(target string, qtype uint16) []RR {
	if answers, _, inZone, _ := s.Zone.Resolve(Question{Name: target, Type: qtype, Class: ClassINET}); inZone {
		return answers // a chain of ExternalNames stops after one more CNAME
	}
	q := &Message{ID: uint16(time.Now().UnixNano()), RecursionDesired: true,
		Questions: []Question{{Name: target, Type: qtype, Class: ClassINET}}}
	resp, err := s.forward(q.Pack())
	if err != nil {
		return nil
	}
	m, err := Parse(resp)
	if err != nil || m.Rcode != RcodeSuccess {
		return nil
	}
	return m.Answers
}

func (s *Server) forward(query []byte) ([]byte, error) {
	if s.Forward != nil {
		return s.Forward(query)
	}
	conn, err := net.Dial("udp", s.Upstream)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(query); err != nil {
		return nil, err
	}
	buf := make([]byte, 65535)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// ListenAndServe serves UDP and TCP on addr until ctx ends.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		pc.Close()
		return err
	}
	go func() {
		<-ctx.Done()
		pc.Close()
		ln.Close()
	}()
	var wg sync.WaitGroup
	wg.Go(func() { s.serveUDP(pc) })
	wg.Go(func() { s.serveTCP(ln) })
	wg.Wait()
	if ctx.Err() != nil {
		return nil
	}
	return errors.New("dns: listener stopped")
}

func (s *Server) serveUDP(pc net.PacketConn) {
	buf := make([]byte, 65535)
	inFlight := make(chan struct{}, maxInFlight)
	for {
		n, client, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		query := append([]byte(nil), buf[:n]...)
		inFlight <- struct{}{} // bounds the goroutines (and upstream sockets) per server
		go func() {
			defer func() { <-inFlight }()
			if reply := s.Handle(query, maxUDP); reply != nil {
				if _, err := pc.WriteTo(reply, client); err != nil {
					log.Printf("dns: reply to %s: %v", client, err)
				}
			}
		}()
	}
}

// serveTCP answers length-prefixed queries (RFC 1035 4.2.2).
func (s *Server) serveTCP(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			for {
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				var size [2]byte
				if _, err := io.ReadFull(conn, size[:]); err != nil {
					return
				}
				query := make([]byte, binary.BigEndian.Uint16(size[:]))
				if _, err := io.ReadFull(conn, query); err != nil {
					return
				}
				reply := s.Handle(query, 0)
				if reply == nil {
					return
				}
				if _, err := conn.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(reply))), reply...)); err != nil {
					return
				}
			}
		}()
	}
}
