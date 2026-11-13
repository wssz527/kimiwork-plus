package byokdns

import (
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func query(id uint16, domain string, typeCode uint16) []byte {
	packet := make([]byte, 12, 512)
	binary.BigEndian.PutUint16(packet[0:2], id)
	binary.BigEndian.PutUint16(packet[2:4], 0x0100)
	binary.BigEndian.PutUint16(packet[4:6], 1)
	for _, label := range splitDomain(domain) {
		packet = append(packet, byte(len(label)))
		packet = append(packet, label...)
	}
	packet = append(packet, 0, 0, 0, 0, 1)
	binary.BigEndian.PutUint16(packet[len(packet)-4:len(packet)-2], typeCode)
	return packet
}

func splitDomain(domain string) []string {
	return strings.Split(domain, ".")
}

func TestReplyMatchesTheOfficialProbe(t *testing.T) {
	response := Reply(query(0x1234, "DeV.mSh.TeAm", 1))
	if response == nil || binary.BigEndian.Uint16(response[0:2]) != 0x1234 || binary.BigEndian.Uint16(response[6:8]) != 1 {
		t.Fatalf("unexpected A response: %x", response)
	}
	if got := net.IP(response[len(response)-4:]).String(); got != Address {
		t.Fatalf("answer = %s", got)
	}
	aaaa := Reply(query(7, Domain, 28))
	if aaaa == nil || binary.BigEndian.Uint16(aaaa[6:8]) != 0 || binary.BigEndian.Uint16(aaaa[2:4])&15 != 0 {
		t.Fatalf("unexpected AAAA response: %x", aaaa)
	}
	other := Reply(query(8, "example.com", 1))
	if other == nil || binary.BigEndian.Uint16(other[2:4])&15 != 5 {
		t.Fatalf("other domain was not refused: %x", other)
	}
}

func TestReplyRejectsMalformedPackets(t *testing.T) {
	valid := query(1, Domain, 1)
	cases := [][]byte{nil, {}, make([]byte, 11), make([]byte, 4097), append([]byte(nil), valid...), append([]byte(nil), valid...), append([]byte(nil), valid...), append([]byte(nil), valid...)}
	binary.BigEndian.PutUint16(cases[4][2:4], 0x8100)
	binary.BigEndian.PutUint16(cases[5][4:6], 2)
	cases[6][12] = 64
	cases[7][12] = '_'
	for index, packet := range cases {
		if Reply(packet) != nil {
			t.Fatalf("case %d received a response", index)
		}
	}
}

func TestPrivateAddress(t *testing.T) {
	for _, value := range []string{"10.0.0.1", "172.16.0.1", "172.31.255.255", "192.168.1.1", "fc00::1", "fd12::1"} {
		if !PrivateAddress(value) {
			t.Fatalf("%s was not private", value)
		}
	}
	for _, value := range []string{"8.8.8.8", net.IP{172, 15, 0, 1}.String(), net.IP{172, 32, 0, 1}.String(), net.IP{192, 167, 0, 1}.String(), "fe80::1", "::ffff:10.0.0.1", "not-an-address"} {
		if PrivateAddress(value) {
			t.Fatalf("%s was private", value)
		}
	}
}

func TestServerAnswersAndReleasesPort(t *testing.T) {
	var server Server
	if err := server.Start(0); err != nil {
		t.Fatal(err)
	}
	port := server.Port()
	if port == 0 {
		t.Fatal("listener port was not assigned")
	}
	if err := server.Start(0); err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(query(42, Domain, 1)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 512)
	count, err := conn.Read(buffer)
	if err != nil || binary.BigEndian.Uint16(buffer[:2]) != 42 {
		t.Fatalf("DNS read failed: count=%d err=%v", count, err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	rebound, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	_ = rebound.Close()
}

func TestServerRejectsInvalidAndBusyPorts(t *testing.T) {
	var server Server
	if err := server.Start(-1); !errors.Is(err, ErrInvalidPort) {
		t.Fatalf("invalid port error = %v", err)
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := server.Start(conn.LocalAddr().(*net.UDPAddr).Port); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("busy port error = %v", err)
	}
}
