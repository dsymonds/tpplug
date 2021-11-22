package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/dsymonds/tpplug/tpplug"
	promrawapi "github.com/prometheus/client_golang/api"
	promclient "github.com/prometheus/client_golang/api/prometheus/v1"
	prommodel "github.com/prometheus/common/model"
	"gopkg.in/yaml.v2"
)

var (
	configFile = flag.String("config_file", "solarctrl.yaml", "configuration `filename`")
	port       = flag.Int("port", 0, "`port` to serve HTTP (optional)")
	vFlag      = flag.Bool("v", false, "be verbose")

	loop      = flag.Duration("loop", 0, "if set, run and evaluate every `period`")
	minToggle = flag.Duration("min_toggle", 5*time.Minute, "minimum time between toggles")
)

func vlogf(format string, args ...interface{}) {
	if !*vFlag {
		return
	}
	log.Printf(format, args...)
}

const (
	// solarQuery is the Prometheus query expression to retrieve the current solar production in Watts as a 1-vector.
	solarQuery = `sum(power_production_watts{job="solarmon"})`
)

type Config struct {
	PrometheusAddr string `yaml:"prometheus_addr"` // URL

	BaselineConsumption Power `yaml:"baseline_consumption"`

	DiscretionaryPlugs []TPPlugSelector `yaml:"discretionary_plugs"`
}

func (cfg Config) Discretionary(p Plug) bool {
	for _, tps := range cfg.DiscretionaryPlugs {
		if tps.Matches(p) {
			return true
		}
	}
	return false
}

type TPPlugSelector struct {
	Alias       string
	Consumption Power
}

func (tps TPPlugSelector) Matches(p Plug) bool { return tps.Alias == p.Alias() }

type Plug struct {
	Raw tpplug.DiscoveryResponse

	// Assumed overrides the power in Raw.
	AssumedPower Power
}

func (p Plug) Addr() *net.UDPAddr { return p.Raw.Addr }
func (p Plug) On() bool           { return p.Raw.System.Info.RelayState == 1 }
func (p Plug) Power() Power {
	if p.AssumedPower > 0 {
		return p.AssumedPower
	}
	return Power(p.Raw.EnergyMeter.Realtime.Power / 1000) // mw -> W
}
func (p Plug) Alias() string { return p.Raw.System.Info.Alias }

func main() {
	flag.Parse()

	var config Config
	configRaw, err := ioutil.ReadFile(*configFile)
	if err != nil {
		log.Fatalf("Reading config file %s: %v", *configFile, err)
	}
	if err := yaml.UnmarshalStrict(configRaw, &config); err != nil {
		log.Fatalf("Parsing config from %s: %v", *configFile, err)
	}

	vlogf("Prometheus at %q", config.PrometheusAddr)
	promClient, err := promrawapi.NewClient(promrawapi.Config{
		Address: config.PrometheusAddr,
	})
	if err != nil {
		log.Fatalf("Creating Prometheus client: %v", err)
	}
	promAPI := promclient.NewAPI(promClient)

	if *port != 0 {
		go func() {
			log.Printf("Serving HTTP on port %d", *port)
			log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), nil))
		}()
	}

	s := newServer(config, promAPI)
	http.Handle("/", s)

	// Evaluate at least once.
	s.evaluate(context.Background())

	if *loop <= 0 {
		return
	}

	for range time.NewTicker(*loop).C {
		s.evaluate(context.Background())
	}
}

type Power int // measured in Watts

func (p Power) String() string {
	if p > 1000 {
		return fmt.Sprintf("%.2fkW", float64(p)/1000)
	}
	return fmt.Sprintf("%dW", p)
}

func solarPower(ctx context.Context, promAPI promclient.API) (Power, error) {
	v, warns, err := promAPI.Query(ctx, solarQuery, time.Now())
	if err != nil {
		return 0, fmt.Errorf("Prometheus query evaluation: %w", err)
	}
	for _, w := range warns {
		vlogf("During Prometheus query evaluation: %s", w)
	}

	if v.Type() != prommodel.ValVector {
		return 0, fmt.Errorf("Prometheus query yielded %v, want vector", v.Type())
	}
	vec := v.(prommodel.Vector)
	if len(vec) != 1 {
		return 0, fmt.Errorf("Prometheus query yielded vector of %d values, want 1", len(vec))
	}
	return Power(vec[0].Value), nil
}

type server struct {
	config  Config
	promAPI promclient.API

	// State updated with each evaluation.
	mu          sync.Mutex
	lastLog     bytes.Buffer
	lastToggles map[string]time.Time // plug name => time
}

func newServer(config Config, promAPI promclient.API) *server {
	return &server{
		config:  config,
		promAPI: promAPI,

		lastToggles: make(map[string]time.Time),
	}
}

func (s *server) evaluate(ctx context.Context) (err error) {
	var evalLog bytes.Buffer
	elogf := func(format string, args ...interface{}) {
		vlogf(format, args...)
		fmt.Fprintf(&evalLog, format+"\n", args...)
	}
	defer func() {
		if err != nil {
			elogf("ERROR: %v", err)
		}
		s.mu.Lock()
		s.lastLog = evalLog
		s.mu.Unlock()
	}()
	elogf("Starting evaluation at %v", time.Now().Format(time.RFC3339))

	// Fetch latest solar production.
	solar, err := solarPower(ctx, s.promAPI)
	if err != nil {
		return fmt.Errorf("querying solar power: %w", err)
	}
	elogf("Current solar: %v", solar)

	plugs, err := allPlugs(ctx)
	if err != nil {
		return err
	}

	// Enumerate the plugs. Compute how much spare solar there is,
	// and collate the discretionary plugs at the same time.
	var discPlugs []Plug
	spareSolar := solar - s.config.BaselineConsumption
	for _, p := range plugs {
		spareSolar -= p.Power()

		var sel *TPPlugSelector
		for _, tps := range s.config.DiscretionaryPlugs {
			if tps.Matches(p) {
				sel = &tps
				break
			}
		}
		if sel == nil {
			continue
		}

		if !p.On() {
			// This plug is off.
			// Fill in the configured consumption value so we can use it below.
			p.AssumedPower = sel.Consumption
		}
		discPlugs = append(discPlugs, p)
	}
	elogf("Found %d plugs, %d discretionary", len(plugs), len(discPlugs))
	elogf("Spare solar: %v", spareSolar)

	// See if there are any discretionary plugs to toggle.
	// TODO: sort them first so this is deterministic.
	for _, p := range discPlugs {
		// If this plug was toggled too recently, don't consider it.
		s.mu.Lock()
		last, ok := s.lastToggles[p.Alias()]
		s.mu.Unlock()
		if ok && time.Since(last) < *minToggle {
			elogf("Plug %q toggled too recently; leaving it alone", p.Alias())
			continue
		}

		if spareSolar < 0 && p.On() {
			elogf("Turning off %q at %v to save %v", p.Alias(), p.Addr(), p.Power())
			log.Printf("Turning off %q at %v", p.Alias(), p.Addr())
			spareSolar += p.Power()
		} else if spareSolar > p.Power() && !p.On() {
			elogf("Turning on %q at %v, estimated to use %v", p.Alias(), p.Addr(), p.Power())
			log.Printf("Turning on %q at %v", p.Alias(), p.Addr())
			spareSolar -= p.Power()
		} else {
			continue
		}

		newState := 1 - p.Raw.System.Info.RelayState
		err := tpplug.SetRelayState(ctx, p.Addr(), newState)
		if err != nil {
			elogf("Failed to toggle %q: %v", p.Alias(), err)
			log.Printf("Failed to toggle %q: %v", p.Alias(), err)
			continue
		}
		s.mu.Lock()
		s.lastToggles[p.Alias()] = time.Now()
		s.mu.Unlock()
	}
	return nil
}

func allPlugs(ctx context.Context) ([]Plug, error) {
	// Discover all plugs, with a 5s timeout.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	drs, err := tpplug.Discover(ctx)
	if err != nil {
		return nil, fmt.Errorf("discovering plugs: %w", err)
	}
	var plugs []Plug
	for _, dr := range drs {
		plugs = append(plugs, Plug{
			Raw: dr,
		})
	}
	return plugs, nil
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	data := struct {
		LastLog     string
		LastToggles map[string]time.Time
	}{
		LastLog:     "never evaluated",
		LastToggles: make(map[string]time.Time),
	}
	s.mu.Lock()
	if s.lastLog.Len() > 0 {
		data.LastLog = s.lastLog.String()
	}
	for name, t := range s.lastToggles {
		data.LastToggles[name] = t
	}
	s.mu.Unlock()

	var buf bytes.Buffer
	if err := serveTmpl.Execute(&buf, data); err != nil {
		log.Printf("Internal error rendering template: %v", err)
		http.Error(w, "rendering template: "+err.Error(), 500)
		return
	}
	io.Copy(w, &buf)
}

var serveTmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"roughSince": func(t time.Time) string {
		d := time.Since(t).Truncate(1 * time.Second)
		return d.String()
	},
}).Parse(`
<!doctype html><html lang="en">
<head><title>solarctrl</title></head>
<body>

<h1>solarctrl</h1>

Last evaluation:
<pre>
{{.LastLog}}
</pre>

Last toggles:
<dl>
{{range $name, $t := .LastToggles}}
<dt>{{$name}}</dt>
<dd>{{roughSince $t}} ago</dd>
{{end}}
</dl>

</body>
</html>
`))
