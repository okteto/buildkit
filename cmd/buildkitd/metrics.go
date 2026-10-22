package main

import (
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/moby/buildkit/control"
	"github.com/moby/buildkit/util/bklog"
	"github.com/moby/buildkit/util/metrics"
)

func setupMetricsHandlers(addr string, controller *control.Controller) error {
	// Initialize cgroup configuration
	cgroupConfig, err := metrics.InitCgroupConfig()
	if err != nil {
		bklog.L.Errorf("failed to initialize cgroup config: %v", err)
		return nil
	}

	// Create function to get active builds from controller
	getActiveBuilds := func() float64 {
		return float64(controller.GetActiveBuilds())
	}

	// Create metrics server
	metricsServer := metrics.NewServer(getActiveBuilds, cgroupConfig)

	// Setup HTTP server with metrics endpoint
	m := http.NewServeMux()
	m.Handle("/metrics", metricsServer.Handler())

	if !strings.Contains(addr, "://") {
		addr = "tcp://" + addr
	}
	l, err := getListener(addr, os.Getuid(), os.Getgid(), "", nil, false)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              l.Addr().String(),
		Handler:           m,
		ReadHeaderTimeout: time.Minute,
	}
	bklog.L.Infof("okteto metrics handlers listening at %s", addr)
	go func() {
		if err := server.Serve(l); err != nil {
			bklog.L.Errorf("failed to serve okteto metrics handlers: %v", err)
		}
	}()
	return nil
}
