package protocol

import (
	"encoding/binary"
	"fmt"
	"io"
	"paqet/internal/conf"
	"paqet/internal/tnet"
)

type PType = byte

const (
	PPING PType = 0x01
	PPONG PType = 0x02
	PTCPF PType = 0x03
	PTCP  PType = 0x04
	PUDP  PType = 0x05
)

type Proto struct {
	Type PType
	Addr *tnet.Addr
	TCPF []conf.TCPF
}

func (p *Proto) Read(r io.Reader) error {
	// Read type byte
	if err := binary.Read(r, binary.BigEndian, &p.Type); err != nil {
		return err
	}

	switch p.Type {
	case PPING, PPONG:
		// No payload needed — just the type byte
		return nil

	case PTCPF:
		// Read TCPF count
		var count uint8
		if err := binary.Read(r, binary.BigEndian, &count); err != nil {
			return err
		}
		p.TCPF = make([]conf.TCPF, count)
		for i := range count {
			var flags uint16
			if err := binary.Read(r, binary.BigEndian, &flags); err != nil {
				return err
			}
			p.TCPF[i] = decodeTCPF(flags)
		}
		return nil

	case PTCP, PUDP:
		// Read address
		addr, err := readAddr(r)
		if err != nil {
			return err
		}
		p.Addr = addr
		return nil
	}

	return fmt.Errorf("unknown protocol type: 0x%02x", p.Type)
}

func (p *Proto) Write(w io.Writer) error {
	// Write type byte
	if err := binary.Write(w, binary.BigEndian, p.Type); err != nil {
		return err
	}

	switch p.Type {
	case PPING, PPONG:
		// Just the type byte — 1 byte total instead of ~50 with gob
		return nil

	case PTCPF:
		count := uint8(len(p.TCPF))
		if err := binary.Write(w, binary.BigEndian, count); err != nil {
			return err
		}
		for _, f := range p.TCPF {
			if err := binary.Write(w, binary.BigEndian, encodeTCPF(f)); err != nil {
				return err
			}
		}
		return nil

	case PTCP, PUDP:
		return writeAddr(w, p.Addr)
	}

	return fmt.Errorf("unknown protocol type: 0x%02x", p.Type)
}

// encodeTCPF packs TCP flags into a uint16 bitmask.
func encodeTCPF(f conf.TCPF) uint16 {
	var flags uint16
	if f.FIN {
		flags |= 1 << 0
	}
	if f.SYN {
		flags |= 1 << 1
	}
	if f.RST {
		flags |= 1 << 2
	}
	if f.PSH {
		flags |= 1 << 3
	}
	if f.ACK {
		flags |= 1 << 4
	}
	if f.URG {
		flags |= 1 << 5
	}
	if f.ECE {
		flags |= 1 << 6
	}
	if f.CWR {
		flags |= 1 << 7
	}
	if f.NS {
		flags |= 1 << 8
	}
	return flags
}

// decodeTCPF unpacks a uint16 bitmask into TCP flags.
func decodeTCPF(flags uint16) conf.TCPF {
	return conf.TCPF{
		FIN: flags&(1<<0) != 0,
		SYN: flags&(1<<1) != 0,
		RST: flags&(1<<2) != 0,
		PSH: flags&(1<<3) != 0,
		ACK: flags&(1<<4) != 0,
		URG: flags&(1<<5) != 0,
		ECE: flags&(1<<6) != 0,
		CWR: flags&(1<<7) != 0,
		NS:  flags&(1<<8) != 0,
	}
}

// writeAddr serializes a tnet.Addr to binary.
// Format: [1 byte present][1 byte hostLen][host bytes][2 byte port]
func writeAddr(w io.Writer, addr *tnet.Addr) error {
	if addr == nil {
		return binary.Write(w, binary.BigEndian, uint8(0)) // not present
	}
	if err := binary.Write(w, binary.BigEndian, uint8(1)); err != nil { // present
		return err
	}
	hostBytes := []byte(addr.Host)
	hostLen := uint8(len(hostBytes))
	if err := binary.Write(w, binary.BigEndian, hostLen); err != nil {
		return err
	}
	if hostLen > 0 {
		if _, err := w.Write(hostBytes); err != nil {
			return err
		}
	}
	return binary.Write(w, binary.BigEndian, uint16(addr.Port))
}

// readAddr deserializes a tnet.Addr from binary.
func readAddr(r io.Reader) (*tnet.Addr, error) {
	var present uint8
	if err := binary.Read(r, binary.BigEndian, &present); err != nil {
		return nil, err
	}
	if present == 0 {
		return nil, nil
	}
	var hostLen uint8
	if err := binary.Read(r, binary.BigEndian, &hostLen); err != nil {
		return nil, err
	}
	var host string
	if hostLen > 0 {
		hostBytes := make([]byte, hostLen)
		if _, err := io.ReadFull(r, hostBytes); err != nil {
			return nil, err
		}
		host = string(hostBytes)
	}
	var port uint16
	if err := binary.Read(r, binary.BigEndian, &port); err != nil {
		return nil, err
	}
	return &tnet.Addr{Host: host, Port: int(port)}, nil
}
