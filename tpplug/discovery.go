package tpplug

type DiscoveryMessage struct {
	System struct {
		Info struct {
			Model      string `json:"model,omitempty"` // e.g. "HS110(AU)"
			MAC        string `json:"mac,omitempty"`
			Alias      string `json:"alias,omitempty"`       // Human-readable name.
			RelayState int    `json:"relay_state,omitempty"` // 0 = off?
			// Other keys: sw_ver, hw_ver, type, dev_name, on_time, active_mode
			//	feature, updating, icon_hash, rssi, led_off, longitude_i, latitude_i
			//	hwId, fwId, deviceId, oemId, next_action, err_code
		} `json:"get_sysinfo"`
	} `json:"system"`
	EnergyMeter struct {
		Realtime struct {
			Voltage int `json:"voltage_mv,omitempty"` // mV
			Current int `json:"current_ma,omitempty"` // mA
			Power   int `json:"power_mw,omitempty"`   // mW
			// Other keys: total_wh, err_code
		} `json:"get_realtime"`
	} `json:"emeter"`
}
