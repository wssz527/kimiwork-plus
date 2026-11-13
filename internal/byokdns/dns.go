package byokdns

import (
	"encoding/binary"
	"errors"
	"net"
	"regexp"
	"strings"
	"sync"
)

const (
	Domain  = "dev.msh.team"
	Address = "10.255.255.254"
	DNSPort = 53535
)

var (
	ErrUnavailable = errors.New("native_byok_dns_unavailable")
	ErrInvalidPort = errors.New("invalid_dns_port")
)

var dnsLabel = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

func PrivateAddress(value string) bool {
	ip := net.ParseIP(value)
	if ip == nil {
		return false
	}
	if strings.Contains(value, ":") {
		ipv6 := ip.To16()
		return ipv6 != nil && ipv6[0]&0xfe == 0xfc
	}
	ipv4 := ip.To4()
	return ipv4 != nil && (ipv4[0] == 10 || ipv4[0] == 172 && ipv4[1] >= 16 && ipv4[1] <= 31 || ipv4[0] == 192 && ipv4[1] == 168)
}

func Reply(packet []byte) []byte {
	if len(packet) < 12 || len(packet) > 4096 || binary.BigEndian.Uint16(packet[2:4])&0x8000 != 0 {
		return nil
	}
	flags := binary.BigEndian.Uint16(packet[2:4])
	if binary.BigEndian.Uint16(packet[4:6]) != 1 || flags&0x7800 != 0 {
		return nil
	}
	offset := 12
	labels := []string{}
	for offset < len(packet) {
		length := int(packet[offset])
		offset++
		if length == 0 {
			break
		}
		if length > 63 || offset+length > len(packet) {
			return nil
		}
		label := string(packet[offset : offset+length])
		if !dnsLabel.MatchString(label) {
			return nil
		}
		labels = append(labels, label)
		offset += length
		if len(strings.Join(labels, ".")) > 253 {
			return nil
		}
	}
	if offset+4 > len(packet) {
		return nil
	}
	typeCode := binary.BigEndian.Uint16(packet[offset : offset+2])
	classCode := binary.BigEndian.Uint16(packet[offset+2 : offset+4])
	allowed := strings.ToLower(strings.Join(labels, ".")) == Domain && classCode == 1 && (typeCode == 1 || typeCode == 28)
	answer := allowed && typeCode == 1
	question := packet[12 : offset+4]
	response := make([]byte, 12, 12+len(question)+16)
	binary.BigEndian.PutUint16(response[0:2], binary.BigEndian.Uint16(packet[0:2]))
	binary.BigEndian.PutUint16(response[2:4], 0x8400|flags&0x0100)
	if !allowed {
		binary.BigEndian.PutUint16(response[2:4], 0x8405|flags&0x0100)
	}
	binary.BigEndian.PutUint16(response[4:6], 1)
	if answer {
		binary.BigEndian.PutUint16(response[6:8], 1)
	}
	response = append(response, question...)
	if !answer {
		return response
	}
	record := make([]byte, 16)
	binary.BigEndian.PutUint16(record[0:2], 0xc00c)
	binary.BigEndian.PutUint16(record[2:4], 1)
	binary.BigEndian.PutUint16(record[4:6], 1)
	binary.BigEndian.PutUint32(record[6:10], 1)
	binary.BigEndian.PutUint16(record[10:12], 4)
	copy(record[12:], net.ParseIP(Address).To4())
	return append(response, record...)
}

type Server struct {
	mu        sync.Mutex
	conn      *net.UDPConn
	requested int
}

func (s *Server) Start(port int) error {
	if port < 0 || port > 65535 {
		return ErrInvalidPort
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil && s.requested == port {
		return nil
	}
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
	if err != nil {
		return ErrUnavailable
	}
	s.conn = conn
	s.requested = port
	go s.serve(conn)
	return nil
}

func (s *Server) serve(conn *net.UDPConn) {
	buffer := make([]byte, 4097)
	for {
		count, sender, err := conn.ReadFromUDP(buffer)
		if err != nil {
			return
		}
		if response := Reply(buffer[:count]); response != nil {
			_, _ = conn.WriteToUDP(response, sender)
		}
	}
}

func (s *Server) Port() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return 0
	}
	return s.conn.LocalAddr().(*net.UDPAddr).Port
}

func (s *Server) Close() error {
	s.mu.Lock()
	conn := s.conn
	s.conn = nil
	s.mu.Unlock()
	if conn == nil {
		return nil
	}
	return conn.Close()
}
