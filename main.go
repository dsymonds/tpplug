package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/dsymonds/tpplug/tpplug"
)

var (
	port     = flag.Int("port", 0, "port to run on")
	scanTime = flag.Duration("scan_time", 2*time.Second, "how long to wait for discovery")
	history  = flag.Duration("history", 10*time.Minute, "how long to keep trying to contact a plug that stopped responding")
)

func main() {
	flag.Parse()

	prometheus.MustRegister(newDataCollector())

	http.Handle("/metrics", promhttp.Handler())
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), nil))
}

// dataCollector implements prometheus.Collector.
type dataCollector struct {
	mu   sync.Mutex
	prev map[string]macInfo
}

var (
	okDesc = prometheus.NewDesc("ok",
		"Whether the listener is working",
		nil, nil)
	powerDesc = prometheus.NewDesc("power_mw",
		"Power (mW)",
		[]string{"mac", "ip", "name"}, nil)
	undiscoveredDesc = prometheus.NewDesc("undiscovered",
		"Count of undiscovered plugs that nonetheless respond to queries",
		nil, nil)
)

func newDataCollector() *dataCollector {
	dc := &dataCollector{}
	return dc
}

func (dc *dataCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- okDesc
	ch <- powerDesc
	ch <- undiscoveredDesc
}

func (dc *dataCollector) Collect(ch chan<- prometheus.Metric) {
	var ok float64
	if err := dc.collect(ch); err != nil {
		log.Printf("Collecting: %v", err)
	} else {
		ok = 1
	}
	ch <- prometheus.MustNewConstMetric(
		okDesc, prometheus.GaugeValue, ok)
}

// macInfo represents a previously seen plug.
type macInfo struct {
	addr *net.UDPAddr
	t    time.Time
}

func (dc *dataCollector) collect(ch chan<- prometheus.Metric) error {
	ctx, cancel := context.WithTimeout(context.Background(), *scanTime)
	defer cancel()

	sendPower := func(state tpplug.State, addr *net.UDPAddr) {
		info := state.System.Info
		rt := state.EnergyMeter.Realtime
		//log.Printf("(%s, %s) %q: %.1f W", info.MAC, addr, info.Alias, float64(rt.Power)/1000)

		ch <- prometheus.MustNewConstMetric(
			powerDesc, prometheus.GaugeValue,
			float64(rt.Power),
			info.MAC, addr.IP.String(), info.Alias)
	}

	drs, err := tpplug.Discover(ctx)
	if err != nil {
		return err
	}
	macs := make(map[string]macInfo)
	now := time.Now()
	for _, dr := range drs {
		macs[dr.State.System.Info.MAC] = macInfo{addr: dr.Addr, t: now}
		sendPower(dr.State, dr.Addr)
	}

	// Query MACs that we saw last time but didn't see this time.
	dc.mu.Lock()
	prev := dc.prev
	dc.mu.Unlock()
	var undiscovered int
	for mac, info := range prev {
		if _, ok := macs[mac]; ok {
			continue
		}
		if now.Sub(info.t) > *history {
			continue
		}

		// TODO: Controllable?
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		state, err := tpplug.Query(ctx, info.addr)
		cancel()
		if err == nil {
			macs[mac] = macInfo{addr: info.addr, t: now}
			sendPower(state, info.addr)
			undiscovered++
		} else {
			// Keep remembering it for now; it'll age out eventually if it never responds.
			macs[mac] = info
		}
	}

	ch <- prometheus.MustNewConstMetric(
		undiscoveredDesc, prometheus.GaugeValue,
		float64(undiscovered))

	// Remember the set of responding plugs and the ones that aren't
	// responding but did within the history interval.
	dc.mu.Lock()
	dc.prev = macs
	dc.mu.Unlock()

	return nil
}
