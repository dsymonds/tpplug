/*
probe queries a specific plug for its state,
and writes it to standard output in its raw JSON format.
*/
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"github.com/dsymonds/tpplug/tpplug"
)

const usage = `
Usage:
	probe [options] <ip>
`

func main() {
	flag.Usage = func() {
		fmt.Fprint(flag.CommandLine.Output(), usage)
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(1)
	}

	ipStr := flag.Arg(0) // TODO: take port too?
	ip := net.ParseIP(ipStr)
	if ip == nil {
		log.Fatalf("Bad IP %q", ipStr)
	}
	addr := &net.UDPAddr{
		IP:   ip,
		Port: 9999,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	raw, err := tpplug.RawQuery(ctx, addr)
	if err != nil {
		log.Fatalf("Querying %v: %v", addr, err)
	}
	os.Stdout.Write(raw)
}
