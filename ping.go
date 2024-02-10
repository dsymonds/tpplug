//go:build ignore
// +build ignore

package main

import (
	"context"
	"flag"
	"log"
	"net"
	"strings"
	"time"

	"github.com/dsymonds/tpplug/tpplug"
)

func main() {
	flag.Parse()

	const wait = 5 * time.Second
	ctx, _ := context.WithTimeout(context.Background(), wait)

	if flag.NArg() == 0 {
		log.Printf("Discovering TP-Link smart plugs over the next %v ...", wait)
		drs, err := tpplug.Discover(ctx)
		if err != nil {
			log.Fatalf("Discover: %v", err)
		}
		log.Printf("Discovered %d smart plugs:", len(drs))
		for _, dr := range drs {
			logMsg(dr.Addr, dr.State)
		}
	} else {
		ip := net.ParseIP(flag.Arg(0))
		if ip == nil {
			log.Fatalf("Bad IP: %q", flag.Arg(0))
		}
		addr := &net.UDPAddr{IP: ip, Port: 9999} // TODO: maybe parse port too?
		state, err := tpplug.Query(ctx, addr)
		if err != nil {
			log.Fatalf("Querying %v: %v", addr, err)
		}
		logMsg(addr, state)
	}
}

func logMsg(addr *net.UDPAddr, state tpplug.State) {
	info := state.System.Info
	rt := state.EnergyMeter.Realtime
	log.Printf("\t(%s %s) %s %q: %.1f W", info.MAC, addr.IP, info.Model, info.Alias, float64(rt.Power)/1000)

	// Toggle the relay state of a smart plug (on => off or off => on).
	if strings.HasPrefix(info.Alias, "XYZ") { // TODO: swap this for something relevant.
		ctx, _ := context.WithTimeout(context.Background(), 1*time.Second)
		log.Printf("\t==> Setting relay state!")
		err := tpplug.SetRelayState(ctx, addr, 1-info.RelayState)
		if err != nil {
			log.Printf("\t==> Setting relay state failed: %v", err)
		}
	}
}
