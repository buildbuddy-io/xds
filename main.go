package main

import (
	"context"
	"flag"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/wongnai/xds/internal/di"
	"github.com/wongnai/xds/meter"
	"github.com/wongnai/xds/snapshot"
	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
	"k8s.io/klog/v2"
)

func main() {
	klog.InitFlags(nil)

	var statsIntervalInSeconds int64
	flag.CommandLine.Int64Var(&statsIntervalInSeconds, "statsinterval", 300, "stats update interval in seconds")
	subZoneLabel := flag.String("sub-zone-label", snapshot.DefaultSubZoneLabel,
		"Kubernetes node label read as sub-zone when a Service requests sub_zone locality")
	flag.Parse()

	ctx := context.Background()

	meter.InstallPromExporter()

	servers, stop, err := di.InitializeServer(context.Background(), statsIntervalInSeconds, snapshot.SubZoneLabel(*subZoneLabel))
	if err != nil {
		klog.Fatal(err)
	}

	lc := net.ListenConfig{}
	lis, err := lc.Listen(ctx, "tcp", ":5000") //nolint:gosec // We're not using TLS
	if err != nil {
		klog.Fatal(err)
	}
	go func() {
		err = servers.GrpcServer.Serve(lis)
		if err != nil {
			klog.Fatal(err)
		}
	}()
	klog.Infoln("Server started")

	sigchan := make(chan os.Signal, 1)
	signal.Notify(sigchan, syscall.SIGINT, syscall.SIGTERM)
	<-sigchan

	klog.Infoln("Stopping...")
	stop()
	lis.Close()
	klog.Infoln("Gracefully stopped")
}
