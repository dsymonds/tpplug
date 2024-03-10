package tpplug

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
)

// writeMsg encrypts a message, and sends it to the UDP target.
// The slice is overwritten in place.
func writeMsg(conn *net.UDPConn, dst *net.UDPAddr, b []byte) error {
	Encrypt(b)

	if _, err := conn.WriteToUDP(b, dst); err != nil {
		return fmt.Errorf("sending message: %w", err)
	}
	return nil
}

func readMsg(conn *net.UDPConn, scratch []byte) (resp []byte, raddr *net.UDPAddr, err error) {
	nb, remoteAddr, err := conn.ReadFrom(scratch)
	if err != nil {
		return nil, nil, fmt.Errorf("reading message: %w", err)
	}
	b := scratch[:nb]
	Decrypt(b)
	return b, remoteAddr.(*net.UDPAddr), nil
}

func RawOp(ctx context.Context, addr *net.UDPAddr, req []byte) ([]byte, error) {
	conn, err := udpConn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if err := writeMsg(conn, addr, req); err != nil {
		return nil, err
	}

	var scratch [4 << 10]byte
	b, _, err := readMsg(conn, scratch[:])
	return b, err
}

func RawJSONOp(ctx context.Context, addr *net.UDPAddr, req, resp interface{}) error {
	b, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encoding JSON request: %w", err)
	}
	out, err := RawOp(ctx, addr, b)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(out, resp); err != nil {
		return fmt.Errorf("decoding JSON request: %w", err)
	}
	return nil
}
