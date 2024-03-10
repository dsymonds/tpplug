package tpplug

import (
	"context"
	"fmt"
	"log"
	"net"
)

type errResponse struct {
	ErrCode int    `json:"err_code,omitempty"`
	ErrMsg  string `json:"err_msg"`
}

func (er errResponse) Err() error {
	if er.ErrCode == 0 { // Assume this means success.
		if er.ErrMsg != "" {
			log.Printf("WARNING: ErrCode=0 but ErrMsg set to %q", er.ErrMsg)
		}
		return nil
	}
	return fmt.Errorf("error code %d (%s)", er.ErrCode, er.ErrMsg)
}

type command struct {
	System *commandSystem `json:"system,omitempty"`
}

type commandSystem struct {
	SetRelayState *setRelayState `json:"set_relay_state,omitempty"`
}

type setRelayState struct {
	// Input.
	State int `json:"state"`

	// Output.
	errResponse
}

func SetRelayState(ctx context.Context, addr *net.UDPAddr, newState int) error {
	// {"system":{"set_relay_state":{"state":1}}}
	req := command{
		System: &commandSystem{
			SetRelayState: &setRelayState{
				State: newState,
			},
		},
	}
	var resp command
	if err := RawJSONOp(ctx, addr, &req, &resp); err != nil {
		return err
	}
	return resp.System.SetRelayState.Err()
}
