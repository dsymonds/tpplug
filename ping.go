package main

import (
	"encoding/json"
	"log"
	"net"
	"time"

	"github.com/dsymonds/tpplug/tpplug"
)

func main() {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		log.Fatalf("net.ListenUDP: %v", err)
	}
	laddr := conn.LocalAddr().(*net.UDPAddr)
	log.Printf("Listening for UDP responses on port %d", laddr.Port)

	discReq := &tpplug.DiscoveryMessage{}
	msg, err := json.Marshal(discReq)
	if err != nil {
		log.Fatalf("Encoding discovery request: %v", err)
	}
	tpplug.Encrypt(msg)

	dst := &net.UDPAddr{
		IP:   net.IPv4(255, 255, 255, 255),
		Port: 9999,
	}
	log.Printf("sending %d byte message", len(msg))
	if _, err := conn.WriteToUDP(msg, dst); err != nil {
		log.Fatalf("conn.WriteToUDP: %v", err)
	}

	// Wait for any responses over the next 5s.
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var scratch [1 << 10]byte
	var nresp int
	for {
		nb, raddr, err := conn.ReadFrom(scratch[:])
		if err != nil {
			if neterr, ok := err.(net.Error); ok && neterr.Timeout() {
				break
			}
			log.Fatalf("conn.ReadFrom: %v", err)
		}
		b := scratch[:nb]
		tpplug.Decrypt(b)
		log.Printf("got back %d bytes from %s", nb, raddr)

		var disc tpplug.DiscoveryMessage
		if err := json.Unmarshal(b, &disc); err != nil {
			log.Printf("ERROR: Parsing response: %v", err)
			continue
		}
		info := disc.System.Info
		rt := disc.EnergyMeter.Realtime
		log.Printf("(%s) %q: %.1f W", info.MAC, info.Alias, float64(rt.Power)/1000)

		nresp++
	}
	log.Printf("Received %d responses.", nresp)
}
