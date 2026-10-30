package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"time"
)

var errNotTLS = errors.New("not a TLS record")

// prefixConn replays bytes already consumed while sniffing the ClientHello.
type prefixConn struct {
	net.Conn
	r io.Reader
}

func (c *prefixConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// readClientHello consumes exactly one TLS record from br and returns the SNI.
// The consumed bytes are returned so the caller can replay them into tls.Server.
func readClientHello(br *bufio.Reader) (sni string, consumed []byte, err error) {
	hdr, err := br.Peek(5)
	if err != nil {
		return "", nil, err
	}
	if hdr[0] != 0x16 {
		return "", nil, errNotTLS
	}
	recLen := int(binary.BigEndian.Uint16(hdr[3:5]))
	if recLen <= 0 || recLen > 1<<15 {
		return "", nil, errNotTLS
	}
	consumed = make([]byte, 5+recLen)
	if _, err = io.ReadFull(br, consumed); err != nil {
		return "", nil, err
	}
	sni, err = parseClientHello(consumed[5:])
	return sni, consumed, err
}

func parseClientHello(p []byte) (string, error) {
	if len(p) < 4 || p[0] != 0x01 {
		return "", errors.New("not a ClientHello")
	}
	n := int(p[1])<<16 | int(p[2])<<8 | int(p[3])
	p = p[4:]
	if len(p) < n {
		return "", errors.New("short ClientHello")
	}
	p = p[:n]
	if len(p) < 2+32+1 {
		return "", errors.New("short ClientHello body")
	}
	p = p[2+32:] // legacy_version + random
	sidLen := int(p[0])
	p = p[1:]
	if len(p) < sidLen+2 {
		return "", errors.New("short session id")
	}
	p = p[sidLen:]
	csLen := int(binary.BigEndian.Uint16(p))
	p = p[2:]
	if len(p) < csLen+1 {
		return "", errors.New("short cipher suites")
	}
	p = p[csLen:]
	compLen := int(p[0])
	p = p[1:]
	if len(p) < compLen+2 {
		return "", errors.New("short compression list")
	}
	p = p[compLen:]
	extLen := int(binary.BigEndian.Uint16(p))
	p = p[2:]
	if len(p) < extLen {
		return "", errors.New("short extensions")
	}
	p = p[:extLen]
	for len(p) >= 4 {
		typ := binary.BigEndian.Uint16(p)
		l := int(binary.BigEndian.Uint16(p[2:4]))
		p = p[4:]
		if l > len(p) {
			break
		}
		if typ == 0x0000 && l >= 5 {
			nl := int(binary.BigEndian.Uint16(p[3:5]))
			if 5+nl <= l {
				return strings.TrimSuffix(string(p[5:5+nl]), "."), nil
			}
		}
		p = p[l:]
	}
	return "", errors.New("no SNI in ClientHello")
}

// setDeadline is a small helper so sniffing cannot hang a worker forever.
func setDeadline(c net.Conn, d time.Duration) {
	_ = c.SetReadDeadline(time.Now().Add(d))
}
