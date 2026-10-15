// Package dns is mini's cluster DNS server, the role CoreDNS and its
// kubernetes plugin play in kubeadm clusters: it answers for Services,
// headless endpoints and Pods under the cluster domain from the API's
// Services and Endpoints, and forwards every other name to the upstream
// resolver. This file holds the DNS wire format (RFC 1035) subset it needs.
package dns

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
)

// Record types and classes.
const (
	TypeA     uint16 = 1
	TypeNS    uint16 = 2
	TypeCNAME uint16 = 5
	TypeSOA   uint16 = 6
	TypePTR   uint16 = 12
	TypeAAAA  uint16 = 28
	TypeSRV   uint16 = 33
	TypeOPT   uint16 = 41
	TypeANY   uint16 = 255
	ClassINET uint16 = 1
)

// Response codes.
const (
	RcodeSuccess        uint8 = 0
	RcodeFormatError    uint8 = 1
	RcodeServerFailure  uint8 = 2
	RcodeNameError      uint8 = 3 // NXDOMAIN
	RcodeNotImplemented uint8 = 4
)

// Message is a DNS message.
type Message struct {
	ID                                                                       uint16
	Response, Authoritative, Truncated, RecursionDesired, RecursionAvailable bool
	Opcode, Rcode                                                            uint8
	Questions                                                                []Question
	Answers, Authority, Additional                                           []RR
}

// Question asks for the records of a type at a name.
type Question struct {
	Name        string
	Type, Class uint16
}

// RR is a resource record; Data is the uncompressed record data.
type RR struct {
	Name        string
	Type, Class uint16
	TTL         uint32
	Data        []byte
}

var (
	errShort   = errors.New("dns: message too short")
	errBadName = errors.New("dns: invalid name")
)

// Limits of RFC 1035.
const (
	maxLabel = 63
	maxName  = 255
)

// Parse decodes a message. Names inside record data are decompressed, so
// records can be copied into other messages.
func Parse(b []byte) (*Message, error) {
	if len(b) < 12 {
		return nil, errShort
	}
	flags := binary.BigEndian.Uint16(b[2:])
	m := &Message{
		ID:                 binary.BigEndian.Uint16(b),
		Response:           flags&0x8000 != 0,
		Opcode:             uint8(flags>>11) & 0xf,
		Authoritative:      flags&0x0400 != 0,
		Truncated:          flags&0x0200 != 0,
		RecursionDesired:   flags&0x0100 != 0,
		RecursionAvailable: flags&0x0080 != 0,
		Rcode:              uint8(flags & 0xf),
	}
	counts := [4]int{}
	for i := range counts {
		counts[i] = int(binary.BigEndian.Uint16(b[4+2*i:]))
	}
	off := 12
	for range counts[0] {
		name, n, err := readName(b, off)
		if err != nil {
			return nil, err
		}
		if n+4 > len(b) {
			return nil, errShort
		}
		m.Questions = append(m.Questions, Question{Name: name, Type: binary.BigEndian.Uint16(b[n:]), Class: binary.BigEndian.Uint16(b[n+2:])})
		off = n + 4
	}
	for i, section := range []*[]RR{&m.Answers, &m.Authority, &m.Additional} {
		for range counts[i+1] {
			rr, n, err := readRR(b, off)
			if err != nil {
				return nil, err
			}
			*section = append(*section, rr)
			off = n
		}
	}
	return m, nil
}

func readRR(b []byte, off int) (RR, int, error) {
	name, n, err := readName(b, off)
	if err != nil {
		return RR{}, 0, err
	}
	if n+10 > len(b) {
		return RR{}, 0, errShort
	}
	rr := RR{Name: name, Type: binary.BigEndian.Uint16(b[n:]), Class: binary.BigEndian.Uint16(b[n+2:]), TTL: binary.BigEndian.Uint32(b[n+4:])}
	length := int(binary.BigEndian.Uint16(b[n+8:]))
	start, end := n+10, n+10+length
	if end > len(b) {
		return RR{}, 0, errShort
	}
	// Names in record data may point anywhere in the message: rewrite them
	// uncompressed.
	switch rr.Type {
	case TypeCNAME, TypeNS, TypePTR:
		target, _, err := readName(b, start)
		if err != nil {
			return RR{}, 0, err
		}
		rr.Data = appendName(nil, target)
	case TypeSRV:
		if length < 7 {
			return RR{}, 0, errShort
		}
		target, _, err := readName(b, start+6)
		if err != nil {
			return RR{}, 0, err
		}
		rr.Data = appendName(append([]byte(nil), b[start:start+6]...), target)
	case TypeSOA:
		mname, n1, err := readName(b, start)
		if err != nil {
			return RR{}, 0, err
		}
		rname, n2, err := readName(b, n1)
		if err != nil || n2+20 > end {
			return RR{}, 0, errShort
		}
		rr.Data = append(appendName(appendName(nil, mname), rname), b[n2:n2+20]...)
	default:
		rr.Data = append([]byte(nil), b[start:end]...)
	}
	return rr, end, nil
}

// readName decodes a possibly compressed name at off and returns it (with
// a trailing dot) and the offset after it.
func readName(b []byte, off int) (string, int, error) {
	var labels []string
	next := -1
	size := 1 // the wire length of the name, root label included
	for jumps := 0; ; {
		if off >= len(b) {
			return "", 0, errShort
		}
		l := int(b[off])
		switch {
		case l == 0:
			if next < 0 {
				next = off + 1
			}
			return strings.Join(labels, ".") + ".", next, nil
		case l&0xc0 == 0xc0:
			if off+1 >= len(b) {
				return "", 0, errShort
			}
			if jumps++; jumps > 32 {
				return "", 0, errors.New("dns: compression loop")
			}
			if next < 0 {
				next = off + 2
			}
			off = int(binary.BigEndian.Uint16(b[off:]) & 0x3fff)
		case l > maxLabel: // 0x40-0xbf are reserved label types
			return "", 0, errBadName
		default:
			if off+1+l > len(b) {
				return "", 0, errShort
			}
			// Bounding the decompressed length keeps a small packet from
			// expanding into huge names (RFC 1035 2.3.4).
			if size += 1 + l; size > maxName {
				return "", 0, errBadName
			}
			labels = append(labels, string(b[off+1:off+1+l]))
			off += 1 + l
		}
	}
}

// appendName encodes name ("a.example." or "a.example") without compression.
func appendName(b []byte, name string) []byte {
	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		if label == "" {
			continue
		}
		b = append(b, byte(len(label)))
		b = append(b, label...)
	}
	return append(b, 0)
}

// Pack encodes the message without name compression.
func (m *Message) Pack() []byte {
	b := make([]byte, 12, 512)
	binary.BigEndian.PutUint16(b, m.ID)
	var flags uint16
	for bit, set := range map[uint16]bool{0x8000: m.Response, 0x0400: m.Authoritative, 0x0200: m.Truncated,
		0x0100: m.RecursionDesired, 0x0080: m.RecursionAvailable} {
		if set {
			flags |= bit
		}
	}
	flags |= uint16(m.Opcode&0xf)<<11 | uint16(m.Rcode&0xf)
	binary.BigEndian.PutUint16(b[2:], flags)
	for i, n := range []int{len(m.Questions), len(m.Answers), len(m.Authority), len(m.Additional)} {
		binary.BigEndian.PutUint16(b[4+2*i:], uint16(n))
	}
	for _, q := range m.Questions {
		b = appendName(b, q.Name)
		b = binary.BigEndian.AppendUint16(b, q.Type)
		b = binary.BigEndian.AppendUint16(b, q.Class)
	}
	for _, section := range [][]RR{m.Answers, m.Authority, m.Additional} {
		for _, rr := range section {
			b = appendName(b, rr.Name)
			b = binary.BigEndian.AppendUint16(b, rr.Type)
			b = binary.BigEndian.AppendUint16(b, rr.Class)
			b = binary.BigEndian.AppendUint32(b, rr.TTL)
			b = binary.BigEndian.AppendUint16(b, uint16(len(rr.Data)))
			b = append(b, rr.Data...)
		}
	}
	return b
}

// A returns an address record.
func A(name string, ttl uint32, ip net.IP) RR {
	return RR{Name: name, Type: TypeA, Class: ClassINET, TTL: ttl, Data: []byte(ip.To4())}
}

// CNAME returns an alias record.
func CNAME(name string, ttl uint32, target string) RR {
	return RR{Name: name, Type: TypeCNAME, Class: ClassINET, TTL: ttl, Data: appendName(nil, target)}
}

// SRV returns a service record.
func SRV(name string, ttl uint32, priority, weight, port uint16, target string) RR {
	data := binary.BigEndian.AppendUint16(nil, priority)
	data = binary.BigEndian.AppendUint16(data, weight)
	data = binary.BigEndian.AppendUint16(data, port)
	return RR{Name: name, Type: TypeSRV, Class: ClassINET, TTL: ttl, Data: appendName(data, target)}
}

// SOA returns the zone's start of authority, as CoreDNS reports it for
// negative answers.
func SOA(zone string, ttl uint32) RR {
	data := appendName(nil, "ns.dns."+zone)
	data = appendName(data, "hostmaster."+zone)
	for _, v := range []uint32{1, 7200, 1800, 86400, ttl} { // serial, refresh, retry, expire, minimum
		data = binary.BigEndian.AppendUint32(data, v)
	}
	return RR{Name: zone, Type: TypeSOA, Class: ClassINET, TTL: ttl, Data: data}
}

// String renders a record for logs and tests, like dig's answer section.
func (rr RR) String() string {
	switch rr.Type {
	case TypeA:
		return fmt.Sprintf("%s %d A %s", rr.Name, rr.TTL, net.IP(rr.Data))
	case TypeCNAME:
		target, _, _ := readName(rr.Data, 0)
		return fmt.Sprintf("%s %d CNAME %s", rr.Name, rr.TTL, target)
	case TypeSRV:
		target, _, _ := readName(rr.Data, 6)
		return fmt.Sprintf("%s %d SRV %d %d %d %s", rr.Name, rr.TTL, binary.BigEndian.Uint16(rr.Data),
			binary.BigEndian.Uint16(rr.Data[2:]), binary.BigEndian.Uint16(rr.Data[4:]), target)
	case TypeSOA:
		return fmt.Sprintf("%s %d SOA", rr.Name, rr.TTL)
	}
	return fmt.Sprintf("%s %d TYPE%d", rr.Name, rr.TTL, rr.Type)
}
