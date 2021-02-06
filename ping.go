// +build ignore

package main

import (
	"context"
	"log"
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
	for _, dr := range drs {
		disc := dr.DiscoveryMessage
		info := disc.System.Info
		rt := disc.EnergyMeter.Realtime
		log.Printf("(%s) %q: %.1f W", info.MAC, info.Alias, float64(rt.Power)/1000)
	}
	log.Printf("Received %d responses.", len(drs))
}
