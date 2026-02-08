package protocol

import (
	"bytes"
	"fmt"
	"paqet/internal/conf"
	"paqet/internal/tnet"
	"testing"
)

func TestProtoRoundTripTCP(t *testing.T) {
	addr := &tnet.Addr{Host: "1.1.1.1", Port: 443}
	orig := Proto{Type: PTCP, Addr: addr}

	var buf bytes.Buffer
	if err := orig.Write(&buf); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	t.Logf("Written bytes (%d): %x", buf.Len(), buf.Bytes())

	var parsed Proto
	if err := parsed.Read(&buf); err != nil {
		t.Fatalf("Read failed: %v", err)
	}

	if parsed.Type != PTCP {
		t.Errorf("Type mismatch: got 0x%02x, want 0x%02x", parsed.Type, PTCP)
	}
	if parsed.Addr == nil {
		t.Fatal("Addr is nil after round-trip")
	}
	if parsed.Addr.Host != "1.1.1.1" {
		t.Errorf("Host mismatch: got %q, want %q", parsed.Addr.Host, "1.1.1.1")
	}
	if parsed.Addr.Port != 443 {
		t.Errorf("Port mismatch: got %d, want %d", parsed.Addr.Port, 443)
	}
}

func TestProtoRoundTripPing(t *testing.T) {
	orig := Proto{Type: PPING}

	var buf bytes.Buffer
	if err := orig.Write(&buf); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	t.Logf("Ping bytes (%d): %x", buf.Len(), buf.Bytes())

	var parsed Proto
	if err := parsed.Read(&buf); err != nil {
		t.Fatalf("Read failed: %v", err)
	}

	if parsed.Type != PPING {
		t.Errorf("Type mismatch: got 0x%02x, want 0x%02x", parsed.Type, PPING)
	}
}

func TestProtoRoundTripTCPF(t *testing.T) {
	flags := []conf.TCPF{
		{PSH: true, ACK: true},
		{SYN: true},
	}
	orig := Proto{Type: PTCPF, TCPF: flags}

	var buf bytes.Buffer
	if err := orig.Write(&buf); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	t.Logf("TCPF bytes (%d): %x", buf.Len(), buf.Bytes())

	var parsed Proto
	if err := parsed.Read(&buf); err != nil {
		t.Fatalf("Read failed: %v", err)
	}

	if parsed.Type != PTCPF {
		t.Errorf("Type mismatch: got 0x%02x", parsed.Type)
	}
	if len(parsed.TCPF) != 2 {
		t.Fatalf("TCPF count: got %d, want 2", len(parsed.TCPF))
	}
	if !parsed.TCPF[0].PSH || !parsed.TCPF[0].ACK {
		t.Errorf("TCPF[0] flags wrong: %+v", parsed.TCPF[0])
	}
	if !parsed.TCPF[1].SYN {
		t.Errorf("TCPF[1] flags wrong: %+v", parsed.TCPF[1])
	}
}

func TestProtoRoundTripHostname(t *testing.T) {
	addr := &tnet.Addr{Host: "www.google.com", Port: 80}
	orig := Proto{Type: PTCP, Addr: addr}

	var buf bytes.Buffer
	if err := orig.Write(&buf); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	t.Logf("Written bytes (%d): %x", buf.Len(), buf.Bytes())

	var parsed Proto
	if err := parsed.Read(&buf); err != nil {
		t.Fatalf("Read failed: %v", err)
	}

	if parsed.Addr == nil {
		t.Fatal("Addr is nil")
	}
	if parsed.Addr.Host != "www.google.com" {
		t.Errorf("Host: got %q, want %q", parsed.Addr.Host, "www.google.com")
	}
	if parsed.Addr.Port != 80 {
		t.Errorf("Port: got %d, want 80", parsed.Addr.Port)
	}
	fmt.Printf("Round-trip OK: %s:%d\n", parsed.Addr.Host, parsed.Addr.Port)
}
