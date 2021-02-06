package tpplug

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
)

func SetRelayState(ctx context.Context, addr *net.UDPAddr, newState int) error {
	conn, err := udpConn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Message structure used for both request and response.
	var req struct {
		System struct {
			SRS struct {
				// Input.
				State int `json:"state"`
				// Output.
				ErrCode int `json:"err_code"`
			} `json:"set_relay_state"`
		} `json:"system"`
	}
	req.System.SRS.State = newState

	msg, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encoding action request: %v", err)
	}
	Encrypt(msg)

	if _, err := conn.WriteToUDP(msg, addr); err != nil {
		return fmt.Errorf("sending action request: %v", err)
	}

	// Wait for any response.
	var scratch [4 << 10]byte
	for {
		nb, _, err := conn.ReadFrom(scratch[:])
		if err != nil {
			if neterr, ok := err.(net.Error); ok && neterr.Timeout() {
				break
			}
			return fmt.Errorf("reading response: %v", err)
		}
		b := scratch[:nb]
		Decrypt(b)

		if err := json.Unmarshal(b, &req); err != nil {
			log.Printf("ERROR: Parsing response: %v", err)
		} else if ec := req.System.SRS.ErrCode; ec != 0 {
			return fmt.Errorf("response with error code %d", ec)
		}
		break
	}

	return nil
}
