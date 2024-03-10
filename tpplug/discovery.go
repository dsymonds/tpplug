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
	b, err := json.Marshal(&State{})
	if err != nil {
		return nil, fmt.Errorf("encoding JSON discovery message: %w", err)
	}
	if err := writeMsg(conn, bcast, b); err != nil {
		return nil, err
	}

	// Wait for any responses.
	var drs []DiscoveryResponse
	var scratch [4 << 10]byte
	for {
		b, raddr, err := readMsg(conn, scratch[:])
		if err != nil {
			var neterr net.Error
			if errors.As(err, &neterr) && neterr.Timeout() {
				// Context timeout reached.
				break
			}
			return nil, err
		}
		var info State
		if err := json.Unmarshal(b, &info); err != nil {
			// One bogus message. Keep going.
			log.Printf("ERROR: %v", err)
			continue
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
	} `json:"emeter,omitempty"`
}

func Query(ctx context.Context, addr *net.UDPAddr) (State, error) {
	var state State
	if err := RawJSONOp(ctx, addr, &state, &state); err != nil {
		return State{}, err
	}
	return state, nil
}
