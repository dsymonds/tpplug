package tpplug

import (
	"context"
	"fmt"
	"log"
	"net"
	"time"
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
	System    *commandSystem `json:"system,omitempty"`
	CountDown *countDown     `json:"count_down,omitempty"`
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

type countDown struct {
	DeleteAllRules *struct{} `json:"delete_all_rules,omitempty"`
	AddRule        *addRule  `json:"add_rule,omitempty"`
}

type addRule struct {
	// Input.
	Enable int `json:"enable"`
	Delay  int `json:"delay"` // seconds?

	Action int    `json:"act"`            // 1 = turn on, 0 = turn off
	Name   string `json:"name,omitempty"` // needed? "turn on"

	// Output.
	errResponse
}

func setRelay(ctx context.Context, addr *net.UDPAddr, newValue, revertValue int, revertDur time.Duration) error {
	revert := revertDur != 0

	s := int(revertDur / time.Second)
	if revert && s <= 0 {
		return fmt.Errorf("negative or too small duration %v (must be at least a second)", revertDur)
	}

	req := command{
		System: &commandSystem{
			SetRelayState: &setRelayState{
				State: newValue,
			},
		},
	}
	if revert {
		req.CountDown = &countDown{
			// There is only one rule permitted at a time.
			// Remove any existing countdown.
			DeleteAllRules: &struct{}{},

			AddRule: &addRule{
				Enable: 1,
				Delay:  s,
				Action: revertValue,
				Name:   "add timer",
			},
		}
	}
	var resp command
	if err := RawJSONOp(ctx, addr, &req, &resp); err != nil {
		return err
	}

	// Two possible failure modes: setting relay state, or adding count down rule.
	if err := resp.System.SetRelayState.Err(); err != nil {
		return err
	}
	if revert {
		return resp.CountDown.AddRule.Err()
	}
	return nil
}

func SetRelayState(ctx context.Context, addr *net.UDPAddr, newState int) error {
	return setRelay(ctx, addr, 1, 0, 0)
}

func SetRelayTemporarily(ctx context.Context, addr *net.UDPAddr, newValue, revertValue int, revertDur time.Duration) error {
	if revertDur <= 0 {
		return fmt.Errorf("duration %v not positive", revertDur)
	}
	return setRelay(ctx, addr, newValue, revertValue, revertDur)
}
