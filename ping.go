// +build ignore

package main

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/dsymonds/tpplug/tpplug"
)

func main() {
	const wait = 5 * time.Second
	ctx, _ := context.WithTimeout(context.Background(), wait)
	log.Printf("Discovering TP-Link smart plugs over the next %v ...", wait)
	drs, err := tpplug.Discover(ctx)
	if err != nil {
		log.Fatalf("Discover: %v", err)
	}
	log.Printf("Discovered %d smart plugs:", len(drs))
	for _, dr := range drs {
		disc := dr.DiscoveryMessage
		info := disc.System.Info
		rt := disc.EnergyMeter.Realtime
		log.Printf("\t(%s) %q: %.1f W", info.MAC, info.Alias, float64(rt.Power)/1000)

		// Toggle the relay state of a smart plug (on => off or off => on).
		if strings.HasPrefix(info.Alias, "XYZ") { // TODO: swap this for something relevant.
			ctx, _ := context.WithTimeout(context.Background(), 1*time.Second)
			log.Printf("\t==> Setting relay state!")
			err := tpplug.SetRelayState(ctx, dr.Addr, 1-info.RelayState)
			if err != nil {
				log.Printf("\t==> Setting relay state failed: %v", err)
			}
		}
	}
}
