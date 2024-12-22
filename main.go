package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
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

	dc := newDataCollector()
	prometheus.MustRegister(dc)

	http.Handle("/", dc)
	http.Handle("/metrics", promhttp.Handler())
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), nil))
}

// dataCollector implements prometheus.Collector.
type dataCollector struct {
	mu   sync.Mutex
	last time.Time
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
	Addr  *net.UDPAddr
	Seen  time.Time
	State tpplug.State
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
		macs[dr.State.System.Info.MAC] = macInfo{Addr: dr.Addr, Seen: now, State: dr.State}
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
		if now.Sub(info.Seen) > *history {
			continue
		}

		// TODO: Controllable?
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		state, err := tpplug.Query(ctx, info.Addr)
		cancel()
		if err == nil {
			macs[mac] = macInfo{Addr: info.Addr, Seen: now, State: state}
			sendPower(state, info.Addr)
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
	dc.last = now
	dc.prev = macs
	dc.mu.Unlock()

	return nil
}

func (dc *dataCollector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var data struct {
		Last    time.Time
		Plugs   map[string]macInfo
		PlugSeq []string // MACs
	}

	dc.mu.Lock()
	data.Last = dc.last
	data.Plugs = dc.prev
	dc.mu.Unlock()

	// Build list of plug MACs, ordered by IP.
	for mac := range data.Plugs {
		data.PlugSeq = append(data.PlugSeq, mac)
	}
	sort.Slice(data.PlugSeq, func(i, j int) bool {
		// lazy sort by IPv4
		ipi := data.Plugs[data.PlugSeq[i]].Addr.IP.To4()
		ipj := data.Plugs[data.PlugSeq[j]].Addr.IP.To4()
		if ipi == nil || ipj == nil {
			return false
		}
		for n := 0; n < 4; n++ {
			if ipi[n] != ipj[n] {
				return ipi[n] < ipj[n]
			}
		}
		return false
	})

	var buf bytes.Buffer
	if err := frontTmpl.Execute(&buf, data); err != nil {
		http.Error(w, "internal error: "+err.Error(), 500)
		return
	}
	io.Copy(w, &buf)
}

var frontTmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"mWtoW": func(x int) float64 { return float64(x) / 1000 },
	"roughSince": func(t time.Time) string {
		d := time.Since(t).Truncate(1 * time.Second)
		return d.String()
	},
}).Parse(`
<!doctype html><html lang="en">
<head><title>tpplug</title></head>
<body>

<h1>tpplug</h1>

Last scan: <b>{{if .Last.IsZero}}never{{else}}{{roughSince .Last}}{{end}}</b>

<table>
<tr>
	<th>MAC</th><th>IP:port</th><th>seen</th>
	<th>model</th><th>name</th><th>last power</th>
</tr>
{{range .PlugSeq}}
{{$p := index $.Plugs .}}
<tr>
	{{/* TODO: $p.State.System.Info.RelayState (0=off, 1=on) */}}
	<td>{{$p.State.System.Info.MAC}}</td>
	<td>{{$p.Addr}}</td>
	<td>{{roughSince $p.Seen}}</td>
	<td>{{$p.State.System.Info.Model}}</td>
	<td>{{$p.State.System.Info.Alias}}</td>
	<td>{{printf "%.1f" (mWtoW $p.State.EnergyMeter.Realtime.Power)}}W</td>
</tr>
{{end}}
</table>

</body>
</html>
`))
