package tpplug

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
)

func udpConn(ctx context.Context) (*net.UDPConn, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return nil, fmt.Errorf("net.ListenUDP: %v", err)
	}
	if d, ok := ctx.Deadline(); ok { // TODO: force a deadline if none provided?
		conn.SetReadDeadline(d)
	}
	return conn, nil
}

// Discover probes the network for smart plugs.
// The provided context controls how long to wait for responses;
// its cancellation or deadline expiry will stop execution of Discover
// but will not return an error.
func Discover(ctx context.Context) ([]DiscoveryResponse, error) {
	conn, err := udpConn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	bcast := &net.UDPAddr{
		IP:   net.IPv4(255, 255, 255, 255),
		Port: 9999,
	}
	if err := writeMsg(conn, bcast, &State{}); err != nil {
		return nil, err
	}

	// Wait for any responses.
	var drs []DiscoveryResponse
	var scratch [4 << 10]byte
	for {
		var info State
		raddr, err := readMsg(conn, scratch[:], &info)
		if err != nil {
			var neterr net.Error
			if errors.As(err, &neterr) && neterr.Timeout() {
				// Context timeout reached.
				break
			}
			var jpe jsonParseError
			if errors.As(err, &jpe) {
				// One bogus message. Keep going.
				log.Printf("ERROR: %v", err)
				continue
			}
			return nil, err
		}
		drs = append(drs, DiscoveryResponse{
			Addr:  raddr,
			State: info,
		})
	}
	return drs, nil
}

type DiscoveryResponse struct {
	Addr  *net.UDPAddr
	State State
}

// State represents a plug's state.
// Its zero value is also used as a query message.
type State struct {
	System struct {
		Info struct {
			Model      string `json:"model,omitempty"` // e.g. "HS110(AU)"
			MAC        string `json:"mac,omitempty"`
			Alias      string `json:"alias,omitempty"`       // Human-readable name.
			RelayState int    `json:"relay_state,omitempty"` // 0 = off, 1 = on
			// Other keys: sw_ver, hw_ver, type, dev_name, on_time, active_mode
			//	feature, updating, icon_hash, rssi, led_off, longitude_i, latitude_i
			//	hwId, fwId, deviceId, oemId, next_action, err_code
		} `json:"get_sysinfo"`
	} `json:"system"`
	EnergyMeter struct {
		Realtime struct {
			Voltage int `json:"voltage_mv,omitempty"` // mV
			Current int `json:"current_ma,omitempty"` // mA
			Power   int `json:"power_mw,omitempty"`   // mW
			// Other keys: total_wh, err_code
		} `json:"get_realtime"`
	} `json:"emeter"`
}

func Query(ctx context.Context, addr *net.UDPAddr) (State, error) {
	conn, err := udpConn(ctx)
	if err != nil {
		return State{}, err
	}
	defer conn.Close()

	if err := writeMsg(conn, addr, &State{}); err != nil {
		return State{}, err
	}

	var scratch [4 << 10]byte
	var state State
	if _, err := readMsg(conn, scratch[:], &state); err != nil {
		return State{}, err
	}
	return state, nil
}

// writeMsg JSON encodes and encrypts a message, and sends it to the UDP target.
func writeMsg(conn *net.UDPConn, dst *net.UDPAddr, msg interface{}) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("encoding message: %w", err)
	}
	Encrypt(b)

	if _, err := conn.WriteToUDP(b, dst); err != nil {
		return fmt.Errorf("sending message: %w", err)
	}
	return nil
}

type jsonParseError struct {
	err error
}

func (jpe jsonParseError) Error() string { return fmt.Sprintf("parsing message: %s", jpe.err.Error()) }

func readMsg(conn *net.UDPConn, scratch []byte, msg interface{}) (raddr *net.UDPAddr, err error) {
	nb, remoteAddr, err := conn.ReadFrom(scratch)
	if err != nil {
		return nil, fmt.Errorf("reading message: %w", err)
	}
	b := scratch[:nb]
	Decrypt(b)
	raddr = remoteAddr.(*net.UDPAddr)

	if err := json.Unmarshal(b, msg); err != nil {
		return raddr, jsonParseError{err}
	}
	return raddr, nil
}
