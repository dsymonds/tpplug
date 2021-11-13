package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"time"

	"github.com/dsymonds/tpplug/tpplug"
	promrawapi "github.com/prometheus/client_golang/api"
	promclient "github.com/prometheus/client_golang/api/prometheus/v1"
	prommodel "github.com/prometheus/common/model"
	"gopkg.in/yaml.v2"
)

var (
	configFile = flag.String("config_file", "solarctrl.yaml", "configuration `filename`")
	vFlag      = flag.Bool("v", false, "be verbose")
	loop       = flag.Duration("loop", 0, "if set, run and evaluate every `period`")
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

	// Evaluate at least once.
	evaluate(context.Background(), config, promAPI)

	if *loop <= 0 {
		return
	}

	for range time.NewTicker(*loop).C {
		evaluate(context.Background(), config, promAPI)
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

func evaluate(ctx context.Context, config Config, promAPI promclient.API) error {
	// Fetch latest solar production.
	solar, err := solarPower(context.Background(), promAPI)
	if err != nil {
		return fmt.Errorf("querying solar power: %w", err)
	}
	vlogf("Current solar: %v", solar)

	plugs, err := allPlugs(ctx)
	if err != nil {
		return err
	}

	// Enumerate the plugs. Compute how much spare solar there is,
	// and collate the discretionary plugs at the same time.
	var discPlugs []Plug
	spareSolar := solar - config.BaselineConsumption
	for _, p := range plugs {
		spareSolar -= p.Power()

		var sel *TPPlugSelector
		for _, tps := range config.DiscretionaryPlugs {
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
	vlogf("Spare solar: %v", spareSolar)

	// See if there are any discretionary plugs to toggle.
	// TODO: sort them first so this is deterministic.
	for _, p := range discPlugs {
		var toggle bool
		if spareSolar < 0 && p.On() {
			log.Printf("Turning off %q at %v", p.Alias(), p.Addr())
			spareSolar += p.Power()
			toggle = true
		} else if spareSolar > 0 && !p.On() {
			log.Printf("Turning on %q at %v", p.Alias(), p.Addr())
			spareSolar -= p.Power()
			toggle = true
		}
		if toggle {
			newState := 1 - p.Raw.System.Info.RelayState
			err := tpplug.SetRelayState(ctx, p.Addr(), newState)
			if err != nil {
				log.Printf("Failed to toggle %q: %v", p.Alias(), err)
			}
		}
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
