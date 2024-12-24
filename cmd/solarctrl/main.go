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
	"sort"
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

	// plugQuery is the Prometheus query expression for the recent smart power consumption.
	// This uses max_over_time to be conservative for high-draw appliances.
	plugQuery = `max_over_time(power_w[5m])`
)

type Config struct {
	PrometheusAddr string `yaml:"prometheus_addr"` // URL

	BaselineConsumption Power `yaml:"baseline_consumption"`

	DiscretionaryPlugs []TPPlugConfig `yaml:"discretionary_plugs"`
}

type TPPlugConfig struct {
	Alias       string
	IP          string
	Consumption Power

	TurnOn  bool `yaml:"turn_on"`
	TurnOff bool `yaml:"turn_off"`
}

type TPPlug struct {
	dp    discPlug
	state tpplug.State

	// Assumed overrides the power in Raw.
	AssumedPower Power
}

func (tp TPPlug) Addr() *net.UDPAddr { return tp.dp.addr }
func (tp TPPlug) On() bool           { return tp.state.System.Info.RelayState == 1 }
func (tp TPPlug) Power() Power {
	if tp.AssumedPower > 0 {
		return tp.AssumedPower
	}
	return Power(tp.state.EnergyMeter.Realtime.Power / 1000) // mw -> W
}

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

	s, err := newServer(config, promAPI)
	if err != nil {
		log.Fatalf("Initialising server: %v", err)
	}
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

type plugData struct {
	Name  string
	MAC   string // if from job=="tpplug"
	Power Power
}

// plugPower fetches the recent smart plug power consumption.
func plugPower(ctx context.Context, promAPI promclient.API) ([]plugData, error) {
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
	var ds []plugData
	for _, s := range vec {
		pd := plugData{
			Name: string(s.Metric["name"]),
			// MAC set below.
			Power: Power(s.Value),
		}
		if s.Metric["job"] == "tpplug" {
			pd.MAC = string(s.Metric["mac"])
		}
		ds = append(ds, pd)
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i].Power > ds[j].Power })
	return ds, nil
}

type server struct {
	config  Config
	dps     []discPlug
	promAPI promclient.API

	// State updated with each evaluation.
	mu          sync.Mutex
	lastLog     bytes.Buffer
	lastToggles map[string]time.Time // plug name => time
	seen        []string             // plug names (discretionary only)

	// Paused plugs.
	pauseMu sync.Mutex
	pauses  map[string]time.Time // plug name => expiry
}

type discPlug struct {
	addr *net.UDPAddr
	cfg  TPPlugConfig
}

func newServer(config Config, promAPI promclient.API) (*server, error) {
	var dps []discPlug
	for _, tp := range config.DiscretionaryPlugs {
		ip := net.ParseIP(tp.IP)
		if ip == nil {
			return nil, fmt.Errorf("bad IP %q", tp.IP)
		}
		dps = append(dps, discPlug{
			addr: &net.UDPAddr{
				IP:   ip,
				Port: 9999, // fixed port
			},
			cfg: tp,
		})
	}

	return &server{
		config:  config,
		dps:     dps,
		promAPI: promAPI,

		lastToggles: make(map[string]time.Time),

		pauses: make(map[string]time.Time),
	}, nil
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
	plugs, err := plugPower(ctx, s.promAPI)
	if err != nil {
		return fmt.Errorf("querying plug power: %w", err)
	}
	plugIndex := make(map[string]*plugData) // keyed by name
	var curUse bytes.Buffer
	for i, pd := range plugs {
		plugIndex[pd.Name] = &plugs[i]
		fmt.Fprintf(&curUse, "\t%q %v\n", pd.Name, pd.Power)
	}
	elogf("Current plug use:\n%s", curUse.String())

	// Query discretionary plugs to check their state.
	discPlugs := make(map[string]TPPlug) // keyed by alias
	for _, dp := range s.dps {
		name := dp.cfg.Alias
		state, err := tpplug.Query(ctx, dp.addr)
		if err != nil {
			elogf("Querying discretionary plug %q (%v): %v", name, dp.addr, err)
			continue
		}
		tp := TPPlug{
			dp:    dp,
			state: state,
		}
		if pd, ok := plugIndex[name]; ok {
			// Use the maximum of its current reported power and the Prometheus-measured power
			// to be conservative for spiky appliances.
			if state.System.Info.RelayState == 1 && pd.Power > tp.Power() {
				elogf("Plug %q nudged up from %v to %v based on recent usage", name, tp.Power(), pd.Power)
				tp.AssumedPower = pd.Power
			}
		} else {
			// Not fatal, but suspicious.
			elogf("WARNING: discretionary plug at %v has configured alias %q that wasn't reported via Prometheus", dp.addr, name)
		}
		discPlugs[name] = tp
		elogf("Discretionary plug %q -> %v", name, tp.Power())
	}

	// Enumerate the plugs. Compute how much spare solar there is.
	spareSolar := solar - s.config.BaselineConsumption
	for _, p := range plugs {
		spareSolar -= p.Power
	}
	elogf("Found %d plugs, %d discretionary", len(plugs), len(discPlugs))
	elogf("Spare solar: %v", spareSolar)

	// See if there are any discretionary plugs to toggle.
	// TODO: sort them first so this is deterministic.
	var seen []string // names
	now := time.Now()
	for _, tp := range discPlugs {
		name := tp.dp.cfg.Alias
		seen = append(seen, name)

		// If this plug was toggled too recently, don't consider it.
		// If it has been paused, also don't consider it.
		s.mu.Lock()
		last, ok := s.lastToggles[name]
		pause, pauseOK := s.pauses[name]
		if pause.Before(now) {
			delete(s.pauses, name)
			pauseOK = false
		}
		s.mu.Unlock()
		if ok && time.Since(last) < *minToggle {
			elogf("Plug %q toggled too recently; leaving it alone", name)
			continue
		}
		if pauseOK {
			elogf("Plug %q has control paused until %v", name, pause)
			continue
		}

		// If the plug is on but can't be turned off (or vice versa),
		// pretend it isn't discretionary.
		if tp.On() && !tp.dp.cfg.TurnOff {
			continue
		}
		if !tp.On() && !tp.dp.cfg.TurnOn {
			continue
		}

		power := tp.Power()
		if !tp.On() && tp.dp.cfg.Consumption > power {
			power = tp.dp.cfg.Consumption
		}
		if spareSolar < 0 && tp.On() {
			elogf("Turning off %q at %v to save %v", name, tp.Addr(), power)
			log.Printf("Turning off %q at %v", name, tp.Addr())
			spareSolar += power
		} else if spareSolar > power && !tp.On() {
			elogf("Turning on %q at %v, estimated to use %v", name, tp.Addr(), power)
			log.Printf("Turning on %q at %v", name, tp.Addr())
			spareSolar -= power
		} else {
			continue
		}

		newState := 1 - tp.state.System.Info.RelayState
		err := tpplug.SetRelayState(ctx, tp.Addr(), newState)
		if err != nil {
			elogf("Failed to toggle %q: %v", name, err)
			log.Printf("Failed to toggle %q: %v", name, err)
			continue
		}
		s.mu.Lock()
		s.lastToggles[name] = time.Now()
		s.mu.Unlock()
	}
	s.mu.Lock()
	s.seen = seen
	s.mu.Unlock()
	return nil
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
		Seen        []string             // names
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
	for _, name := range s.seen {
		if t, ok := s.pauses[name]; ok && t.After(now) {
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
		{{range .Seen}}
		<option value="{{.}}">{{.}}</option>
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
	name := r.PostFormValue("plug")
	d, err := time.ParseDuration(r.PostFormValue("dur"))
	if err != nil {
		http.Error(w, "bad duration: "+err.Error(), http.StatusBadRequest)
		return
	}
	until := time.Now().Add(d)

	// In theory we should do an XSRF check here, but the threat model isn't worth the effort.

	s.pauseMu.Lock()
	s.pauses[name] = until
	s.pauseMu.Unlock()
	log.Printf("Paused %q until %v", name, until)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
