package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	relaydaemon "github.com/libp2p/go-libp2p-relay-daemon"
	libp2phost "github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/routing"
	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	"github.com/libp2p/go-libp2p/p2p/net/connmgr"
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"log"
	"net/http"
	_ "net/http/pprof"
	"time"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
	"github.com/libp2p/go-libp2p/p2p/security/noise"
	libp2ptls "github.com/libp2p/go-libp2p/p2p/security/tls"
)

// Define the names of arguments here.
const (
	NameID     = "id"
	NameConfig = "config"
	NamePSK    = "swarmkey"
)

func main() {
	idPath := flag.String(NameID, "identity", "identity key file path")
	cfgPath := flag.String(NameConfig, "", "json configuration file; empty uses the default configuration")
	pskPath := flag.String(NamePSK, "", "file path to a multicodec-encoded v1 private swarm key")
	flag.Parse()

	cfg, err := relaydaemon.LoadConfig(*cfgPath)
	if err != nil {
		panic(err)
	}
	privk, err := relaydaemon.LoadIdentity(*idPath)
	if err != nil {
		panic(err)
	}

	http.Handle("/metrics", promhttp.Handler())
	go func() {
		http.Handle("/debug/metrics/prometheus", promhttp.Handler())
		log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", cfg.Daemon.PromPort), nil))
	}()

	rcmgr.MustRegisterWith(prometheus.DefaultRegisterer)

	str, err := rcmgr.NewStatsTraceReporter()
	if err != nil {
		log.Fatal(err)
	}
	rmgr, err := rcmgr.NewResourceManager(rcmgr.NewFixedLimiter(rcmgr.DefaultLimits.AutoScale()), rcmgr.WithTraceReporter(str))
	if err != nil {
		log.Fatal(err)
	}

	var opts []libp2p.Option

	opts = append(opts,
		libp2p.Identity(privk),
		libp2p.UserAgent("relayd/1.0"),
		libp2p.DisableRelay(),
		libp2p.ListenAddrStrings(cfg.Network.ListenAddrs...),
		libp2p.ResourceManager(rmgr),
		libp2p.ForceReachabilityPublic(),
		libp2p.EnableNATService(),
		// support TLS connections
		libp2p.Security(libp2ptls.ID, libp2ptls.New),
		// support noise connections
		libp2p.Security(noise.ID, noise.New),
		// support any other default transports (TCP)
		libp2p.DefaultTransports,
	)

	// load PSK if applicable
	if pskPath != nil {
		psk, fprint, err := relaydaemon.LoadSwarmKey(*pskPath)
		if err != nil {
			fmt.Printf("error loading swarm key: %s\n", err.Error())
		}
		if psk != nil {
			fmt.Printf("PSK detected, private identity: %x\n", fprint)
			opts = append(opts, libp2p.PrivateNetwork(psk))
		}
	}

	if len(cfg.Network.AnnounceAddrs) > 0 {
		var announce []ma.Multiaddr
		for _, s := range cfg.Network.AnnounceAddrs {
			a := ma.StringCast(s)
			announce = append(announce, a)
		}
		opts = append(opts,
			libp2p.AddrsFactory(func([]ma.Multiaddr) []ma.Multiaddr {
				return announce
			}),
		)
	} else {
		opts = append(opts,
			libp2p.AddrsFactory(func(addrs []ma.Multiaddr) []ma.Multiaddr {
				announce := make([]ma.Multiaddr, 0, len(addrs))
				for _, a := range addrs {
					if manet.IsPublicAddr(a) {
						announce = append(announce, a)
					}
				}
				return announce
			}),
		)
	}

	cm, err := connmgr.NewConnManager(
		cfg.ConnMgr.ConnMgrLo,
		cfg.ConnMgr.ConnMgrHi,
		connmgr.WithGracePeriod(cfg.ConnMgr.ConnMgrGrace),
	)
	if err != nil {
		panic(err)
	}

	ctx := context.Background()
	var kaddht *dht.IpfsDHT
	newDHT := func(h libp2phost.Host) (routing.PeerRouting, error) {
		var err error
		kaddht, err = dht.New(ctx, h, dht.Mode(dht.ModeServer))
		return kaddht, err
	}

	opts = append(opts,
		libp2p.ConnectionManager(cm),
		libp2p.Routing(newDHT),
	)

	host, err := libp2p.New(opts...)
	if err != nil {
		panic(err)
	}
	err = kaddht.Bootstrap(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Printf("I am %s\n", host.ID())
	fmt.Printf("Public Addresses:\n")
	for _, addr := range host.Addrs() {
		fmt.Printf("\t%s/p2p/%s\n", addr, host.ID())
	}

	go listenPprof(cfg.Daemon.PprofPort)
	time.Sleep(10 * time.Millisecond)

	acl, err := relaydaemon.NewACL(host, cfg.ACL)
	if err != nil {
		panic(err)
	}

	if cfg.RelayV2.Enabled {
		fmt.Printf("Starting RelayV2...\n")
		_, err = relayv2.New(host,
			relayv2.WithResources(cfg.RelayV2.Resources),
			relayv2.WithACL(acl),
			relayv2.WithMetricsTracer(relayv2.NewMetricsTracer(relayv2.WithRegisterer(prometheus.DefaultRegisterer))))
		if err != nil {
			panic(err)
		}
		fmt.Printf("RelayV2 is running!\n")
	}

	select {}
}

func listenPprof(p int) {
	if p == -1 {
		fmt.Printf("The pprof debug is disabled\n")
		return
	}
	addr := fmt.Sprintf("localhost:%d", p)
	fmt.Printf("Registering pprof debug http handler at: http://%s/debug/pprof/\n", addr)
	switch err := http.ListenAndServe(addr, nil); err {
	case nil:
		// all good, server is running and exited normally.
	case http.ErrServerClosed:
		// all good, server was shut down.
	default:
		// error, try another port
		fmt.Printf("error registering pprof debug http handler at: %s: %s\n", addr, err)
		panic(err)
	}
}
