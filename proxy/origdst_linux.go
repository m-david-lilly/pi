//go:build linux

package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"unsafe"

	"golang.org/x/sys/unix"
)

const soOriginalDst = 80 // SO_ORIGINAL_DST, Linux

// originalDst recovers the pre-DNAT destination of a redirected connection.
// Only valid in transparent mode (netfilter REDIRECT); explicit CONNECT uses
// the host from the CONNECT line instead.
func originalDst(c net.Conn) (host string, port int, err error) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return "", 0, fmt.Errorf("not a TCP connection")
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		return "", 0, err
	}
	var serr error
	cerr := rc.Control(func(fd uintptr) {
		var sa unix.RawSockaddrInet4
		size := uint32(unsafe.Sizeof(sa))
		_, _, errno := unix.Syscall6(unix.SYS_GETSOCKOPT, fd, unix.SOL_IP, soOriginalDst,
			uintptr(unsafe.Pointer(&sa)), uintptr(unsafe.Pointer(&size)), 0)
		if errno != 0 {
			serr = errno
			return
		}
		p := (*[2]byte)(unsafe.Pointer(&sa.Port))
		port = int(binary.BigEndian.Uint16(p[:]))
		host = net.IPv4(sa.Addr[0], sa.Addr[1], sa.Addr[2], sa.Addr[3]).String()
	})
	if cerr != nil {
		return "", 0, cerr
	}
	if serr != nil {
		return "", 0, serr
	}
	if port == 0 || host == "0.0.0.0" {
		return "", 0, fmt.Errorf("no original destination")
	}
	return host, port, nil
}
