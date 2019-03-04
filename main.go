package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/dsymonds/tpplug/tpplug"
)

var (
	port = flag.Int("port", 0, "port to run on")
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
		[]string{"mac", "ip"}, nil)
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
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return fmt.Errorf("net.ListenUDP: %v", err)
	}
	defer conn.Close()

	msg, err := json.Marshal(&tpplug.DiscoveryMessage{})
	if err != nil {
		return fmt.Errorf("encoding discovery request: %v", err)
	}
	tpplug.Encrypt(msg)

	dst := &net.UDPAddr{
		IP:   net.IPv4(255, 255, 255, 255),
		Port: 9999,
	}
	if _, err := conn.WriteToUDP(msg, dst); err != nil {
		return fmt.Errorf("conn.WriteToUDP: %v", err)
	}

	// Wait for any responses over the next 5s.
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var scratch [4 << 10]byte
	for {
		nb, raddr, err := conn.ReadFrom(scratch[:])
		if err != nil {
			if neterr, ok := err.(net.Error); ok && neterr.Timeout() {
				break
			}
			return fmt.Errorf("conn.ReadFrom: %v", err)
		}
		b := scratch[:nb]
		tpplug.Decrypt(b)

		var disc tpplug.DiscoveryMessage
		if err := json.Unmarshal(b, &disc); err != nil {
			log.Printf("ERROR: Parsing response: %v", err)
			continue
		}
		info := disc.System.Info
		rt := disc.EnergyMeter.Realtime
		log.Printf("(%s, %s) %q: %.1f W", info.MAC, raddr, info.Alias, float64(rt.Power)/1000)
		ch <- prometheus.MustNewConstMetric(
			powerDesc, prometheus.GaugeValue,
			float64(rt.Power),
			// XXX: drop port from raddr
			info.MAC, raddr.String())
	}
	return nil
}
