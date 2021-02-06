package tpplug

import (
	"context"
	"encoding/json"
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

	msg, err := json.Marshal(&DiscoveryMessage{}) // XXX: really?
	if err != nil {
		return nil, fmt.Errorf("encoding discovery request: %v", err)
	}
	Encrypt(msg)

	dst := &net.UDPAddr{
		IP:   net.IPv4(255, 255, 255, 255),
		Port: 9999,
	}
	//log.Printf("sending %d byte message", len(msg))
	if _, err := conn.WriteToUDP(msg, dst); err != nil {
		return nil, fmt.Errorf("sending discovery request: %v", err)
	}

	// Wait for any responses.
	var drs []DiscoveryResponse
	var scratch [4 << 10]byte
	for {
		nb, raddr, err := conn.ReadFrom(scratch[:])
		if err != nil {
			if neterr, ok := err.(net.Error); ok && neterr.Timeout() {
				break
			}
			return nil, fmt.Errorf("reading response: %v", err)
		}
		b := scratch[:nb]
		Decrypt(b)
		//log.Printf("got back %d bytes from %s", nb, raddr)

		var disc DiscoveryMessage
		if err := json.Unmarshal(b, &disc); err != nil {
			log.Printf("ERROR: Parsing response: %v", err)
			continue
		}
		drs = append(drs, DiscoveryResponse{
			Addr:             raddr.(*net.UDPAddr),
			DiscoveryMessage: disc,
		})
	}
	return drs, nil
}

type DiscoveryResponse struct {
	Addr *net.UDPAddr
	DiscoveryMessage
}

type DiscoveryMessage struct {
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
