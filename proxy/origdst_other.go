//go:build !linux

package main

import (
	"fmt"
	"net"
)

func originalDst(c net.Conn) (string, int, error) {
	return "", 0, fmt.Errorf("transparent mode requires Linux; use the explicit listener")
}
