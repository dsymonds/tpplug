package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/dsymonds/tpplug/tpplug"
)

var (
	port     = flag.Int("port", 0, "port to run on")
	scanTime = flag.Duration("scan_time", 2*time.Second, "how long to wait for discovery")
)

func main() {
	flag.Parse()

	prometheus.MustRegister(newDataCollector())

	http.Handle("/metrics", promhttp.Handler())
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), nil))
}

// dataCollector implements prometheus.Collector.
type dataCollector struct {
}

var (
	okDesc = prometheus.NewDesc("ok",
		"Whether the listener is working",
		nil, nil)
	powerDesc = prometheus.NewDesc("power_mw",
		"Power (mW)",
		[]string{"mac", "ip", "name"}, nil)
)

func newDataCollector() *dataCollector {
	dc := &dataCollector{}
	return dc
}

func (dc *dataCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- okDesc
	ch <- powerDesc
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

func (dc *dataCollector) collect(ch chan<- prometheus.Metric) error {
	ctx, cancel := context.WithTimeout(context.Background(), *scanTime)
	defer cancel()

	drs, err := tpplug.Discover(ctx)
	if err != nil {
		return err
	}
	for _, dr := range drs {
		disc := dr.DiscoveryMessage
		info := disc.System.Info
		rt := disc.EnergyMeter.Realtime
		//log.Printf("(%s, %s) %q: %.1f W", info.MAC, dr.Addr, info.Alias, float64(rt.Power)/1000)

		ip := dr.Addr.IP.String()

		ch <- prometheus.MustNewConstMetric(
			powerDesc, prometheus.GaugeValue,
			float64(rt.Power),
			info.MAC, ip, info.Alias)
	}
	return nil
}
