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
	_ "net/http/pprof"
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
	// This uses avg_over_time to help smooth out abrupt changes.
	solarQuery = `avg_over_time(power_production_watts{job="solarmon"}[5m])`

	// plugQuery is the Prometheus query expression for the recent TPPlug power consumption.
	// This uses max_over_time to be conservative for high-draw appliances.
	plugQuery = `max_over_time(power_mw{job="tpplug"}[5m]) / 1000`
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

	TurnOn  bool `yaml:"turn_on"`
	TurnOff bool `yaml:"turn_off"`
}

func (tps TPPlugSelector) Matches(p Plug) bool { return tps.Alias == p.Alias() }

type Plug struct {
	Raw tpplug.DiscoveryResponse

	// Assumed overrides the power in Raw.
	AssumedPower Power
}

func (p Plug) Addr() *net.UDPAddr { return p.Raw.Addr }
func (p Plug) MAC() string        { return p.Raw.System.Info.MAC }
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

// plugPower fetches the recent TPPlug power consumption, returning a map keyed by MAC address.
func plugPower(ctx context.Context, promAPI promclient.API) (map[string]Power, error) {
	v, warns, err := promAPI.Query(ctx, plugQuery, time.Now())
	if err != nil {
		return nil, fmt.Errorf("Prometheus query evaluation: %w", err)
	}
	for _, w := range warns {
		vlogf("During Prometheus query evaluation: %s", w)
	}

	if v.Type() != prommodel.ValVector {
		return nil, fmt.Errorf("Prometheus query yielded %v, want vector", v.Type())
	}
	vec := v.(prommodel.Vector)
	m := make(map[string]Power, len(vec))
	for _, s := range vec {
		m[string(s.Metric["mac"])] = Power(s.Value)
	}
	return m, nil
}

type server struct {
	config  Config
	promAPI promclient.API

	// State updated with each evaluation.
	mu          sync.Mutex
	lastLog     bytes.Buffer
	lastToggles map[string]time.Time // plug name => time
	seen        map[string]string    // plug MAC => name (discretionary only)

	// Paused plugs.
	pauseMu sync.Mutex
	pauses  map[string]time.Time // plug MAC => expiry
}

func newServer(config Config, promAPI promclient.API) *server {
	return &server{
		config:  config,
		promAPI: promAPI,

		lastToggles: make(map[string]time.Time),

		pauses: make(map[string]time.Time),
	}
}

func (s *server) evaluate(ctx context.Context) (err error) {
	// Don't spend more than 5m on an evaluation. If something gets stuck,
	// hopefully it'll be unstuck by the next evaluation.
	ctx, _ = context.WithTimeout(ctx, 5*time.Minute)

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

	// Fetch latest solar production and TPPlug power consumption.
	solar, err := solarPower(ctx, s.promAPI)
	if err != nil {
		return fmt.Errorf("querying solar power: %w", err)
	}
	elogf("Current solar: %v", solar)
	plugUse, err := plugPower(ctx, s.promAPI)
	if err != nil {
		return fmt.Errorf("querying plug power: %w", err)
	}
	elogf("Current plug use: %v", plugUse)

	plugs, err := allPlugs(ctx)
	if err != nil {
		return err
	}

	// Enumerate the plugs. Compute how much spare solar there is,
	// and collate the discretionary plugs at the same time.
	var discPlugs []Plug
	spareSolar := solar - s.config.BaselineConsumption
	for _, p := range plugs {
		// Use the maximum of its current reported power and the Prometheus-measured power
		// to be conservative for spiky appliances.
		power := p.Power()
		if pu := plugUse[p.MAC()]; p.On() && pu > power {
			elogf("Plug %q nudged up from %v to %v based on recent usage", p.Alias(), power, pu)
			power = pu
		}

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

		// If the plug is on but can't be turned off (or vice versa),
		// pretend it isn't discretionary.
		if p.On() {
			// This plug is on.
			if !sel.TurnOff {
				continue
			}
		} else {
			// This plug is off.
			if !sel.TurnOn {
				continue
			}
			// Fill in the configured consumption value so we can use it below.
			p.AssumedPower = sel.Consumption
		}
		discPlugs = append(discPlugs, p)
	}
	elogf("Found %d plugs, %d discretionary", len(plugs), len(discPlugs))
	elogf("Spare solar: %v", spareSolar)

	// See if there are any discretionary plugs to toggle.
	// TODO: sort them first so this is deterministic.
	seen := make(map[string]string)
	now := time.Now()
	for _, p := range discPlugs {
		seen[p.MAC()] = p.Alias()

		// If this plug was toggled too recently, don't consider it.
		// If it has been paused, also don't consider it.
		s.mu.Lock()
		last, ok := s.lastToggles[p.Alias()]
		pause, pauseOK := s.pauses[p.MAC()]
		if pause.Before(now) {
			delete(s.pauses, p.MAC())
			pauseOK = false
		}
		s.mu.Unlock()
		if ok && time.Since(last) < *minToggle {
			elogf("Plug %q toggled too recently; leaving it alone", p.Alias())
			continue
		}
		if pauseOK {
			elogf("Plug %q has control paused until %v", p.Alias(), pause)
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
	s.mu.Lock()
	s.seen = seen
	s.mu.Unlock()
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
	switch r.URL.Path {
	default:
		http.NotFound(w, r)
		return
	case "/":
		s.serveFront(w, r)
	case "/pause":
		s.servePause(w, r)
	}
}

func (s *server) serveFront(w http.ResponseWriter, r *http.Request) {
	data := struct {
		LastLog     string
		LastToggles map[string]time.Time
		Seen        map[string]string
		Pauses      map[string]time.Time // name => pause expiry
	}{
		LastLog:     "never evaluated",
		LastToggles: make(map[string]time.Time),
		Pauses:      make(map[string]time.Time),
	}
	now := time.Now()
	s.mu.Lock()
	if s.lastLog.Len() > 0 {
		data.LastLog = s.lastLog.String()
	}
	for name, t := range s.lastToggles {
		data.LastToggles[name] = t
	}
	data.Seen = s.seen
	for mac, name := range s.seen {
		if t, ok := s.pauses[mac]; ok && t.After(now) {
			data.Pauses[name] = t
		}
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
	"roughUntil": func(t time.Time) string {
		d := time.Until(t).Truncate(1 * time.Second)
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

{{with .Pauses}}
Paused control for these plugs:
<ul>
{{range $name, $t := .}}
<li>{{$name}} ({{roughUntil $t}} left)</li>
{{end}}
</ul>
{{end}}

<form action="/pause" method="POST">
	<label for="plug-select">Pause control of plug:</label>
	<select name="plug" id="plug-select">
		{{range $mac, $name := .Seen}}
		<option value="{{$mac}}">{{$name}}</option>
		{{end}}
	</select>
	<label for="duration">for:</label>
	<input type="text" value="2h" name="dur" id="duration">
	<input type="submit" value="Pause">
</form>

</body>
</html>
`))

func (s *server) servePause(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	mac := r.PostFormValue("plug")
	d, err := time.ParseDuration(r.PostFormValue("dur"))
	if err != nil {
		http.Error(w, "bad duration: "+err.Error(), http.StatusBadRequest)
		return
	}
	until := time.Now().Add(d)

	// In theory we should do an XSRF check here, but the threat model isn't worth the effort.

	s.pauseMu.Lock()
	s.pauses[mac] = until
	s.pauseMu.Unlock()
	log.Printf("Paused %s until %v", mac, until)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
