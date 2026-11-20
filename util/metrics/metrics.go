package metrics

import (
	"net/http"

	"github.com/moby/buildkit/util/bklog"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
)

// Server manages the metrics collection and serving
type Server struct {
	cpuPressure      prometheus.Gauge
	cpuUsage         prometheus.Gauge
	cpuThrottled     prometheus.Gauge
	cpuTotal         prometheus.Gauge
	memoryUsage      prometheus.Gauge
	memoryTotal      prometheus.Gauge
	inodesUsage      prometheus.Gauge
	inodesTotal      prometheus.Gauge
	ioPressure       prometheus.Gauge
	diskReadBytes    prometheus.Gauge
	diskWriteBytes   prometheus.Gauge
	diskReadOps      prometheus.Gauge
	diskWriteOps     prometheus.Gauge
	diskDiscardBytes prometheus.Gauge
	diskDiscardOps   prometheus.Gauge
	activeBuilds     prometheus.GaugeFunc
	registry         *prometheus.Registry
	cgroupConfig     *CgroupConfig
}

// NewServer creates a new metrics server
func NewServer(getActiveBuilds func() float64, cgroupConfig *CgroupConfig) *Server {
	s := &Server{
		registry: prometheus.NewRegistry(),
		cpuPressure: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "okteto_build",
			Name:      "cpu_pressure",
			Help:      "Percentage of time with some task waiting for CPU (10 second average)",
		}),
		cpuUsage: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "okteto_build",
			Name:      "cpu_usage_milliseconds_total",
			Help:      "Total CPU time consumed in milliseconds (cumulative of daemon and containers)",
		}),
		cpuThrottled: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "okteto_build",
			Name:      "cpu_throttled_milliseconds_total",
			Help:      "Total time spent throttled in milliseconds (cumulative of daemon and containers)",
		}),
		cpuTotal: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "okteto_build",
			Name:      "cpu_total_millicores",
			Help:      "Total CPU available in millicores",
		}),
		memoryUsage: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "okteto_build",
			Name:      "memory_usage_bytes",
			Help:      "Memory usage in bytes (cumulative of daemon and containers)",
		}),
		memoryTotal: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "okteto_build",
			Name:      "memory_total_bytes",
			Help:      "Total memory available in bytes",
		}),
		inodesUsage: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "okteto_build",
			Name:      "inodes_usage",
			Help:      "Number of inodes used on the buildkit storage volume",
		}),
		inodesTotal: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "okteto_build",
			Name:      "inodes_total",
			Help:      "Total inodes available on the buildkit storage volume",
		}),
		ioPressure: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "okteto_build",
			Name:      "io_pressure",
			Help:      "Percentage of time with some task waiting for I/O (10 second average)",
		}),
		diskReadBytes: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "okteto_build",
			Name:      "disk_read_bytes_total",
			Help:      "Total bytes read from storage (cumulative of daemon and containers)",
		}),
		diskWriteBytes: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "okteto_build",
			Name:      "disk_write_bytes_total",
			Help:      "Total bytes written to storage (cumulative of daemon and containers)",
		}),
		diskReadOps: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "okteto_build",
			Name:      "disk_read_ops_total",
			Help:      "Total read operations on storage (cumulative of daemon and containers)",
		}),
		diskWriteOps: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "okteto_build",
			Name:      "disk_write_ops_total",
			Help:      "Total write operations on storage (cumulative of daemon and containers)",
		}),
		diskDiscardBytes: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "okteto_build",
			Name:      "disk_discard_bytes_total",
			Help:      "Total bytes discarded (TRIM) on storage (cumulative of daemon and containers)",
		}),
		diskDiscardOps: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "okteto_build",
			Name:      "disk_discard_ops_total",
			Help:      "Total discard operations on storage (cumulative of daemon and containers)",
		}),
		activeBuilds: prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: "okteto_build",
			Name:      "active_builds",
			Help:      "Number of currently active builds",
		}, getActiveBuilds),
		cgroupConfig: cgroupConfig,
	}

	s.registry.MustRegister(
		s.cpuPressure,
		s.cpuUsage,
		s.cpuThrottled,
		s.cpuTotal,
		s.memoryUsage,
		s.memoryTotal,
		s.inodesUsage,
		s.inodesTotal,
		s.ioPressure,
		s.diskReadBytes,
		s.diskWriteBytes,
		s.diskReadOps,
		s.diskWriteOps,
		s.diskDiscardBytes,
		s.diskDiscardOps,
		s.activeBuilds,
	)

	return s
}

// Gather implements prometheus.Gatherer interface
func (s *Server) Gather() ([]*dto.MetricFamily, error) {
	// Get current container stats
	containerStats := getContainerStats(s.cgroupConfig)

	s.cpuPressure.Set(containerStats.CPUPressure)
	s.cpuUsage.Set(float64(containerStats.CPUUsage))
	s.cpuThrottled.Set(float64(containerStats.CPUThrottled))
	s.cpuTotal.Set(float64(containerStats.CPUTotal))
	s.memoryUsage.Set(float64(containerStats.MemoryUsage))
	s.memoryTotal.Set(float64(containerStats.MemoryTotal))
	s.inodesUsage.Set(float64(containerStats.InodesUsage))
	s.inodesTotal.Set(float64(containerStats.InodesTotal))
	s.ioPressure.Set(containerStats.IOPressure)
	s.diskReadBytes.Set(float64(containerStats.DiskReadBytes))
	s.diskWriteBytes.Set(float64(containerStats.DiskWriteBytes))
	s.diskReadOps.Set(float64(containerStats.DiskReadOps))
	s.diskWriteOps.Set(float64(containerStats.DiskWriteOps))
	s.diskDiscardBytes.Set(float64(containerStats.DiskDiscardBytes))
	s.diskDiscardOps.Set(float64(containerStats.DiskDiscardOps))

	bklog.L.Debugf("BuildKit metrics: cpu_pressure=%.2f%%, cpu_usage=%d ms, cpu_throttled=%d ms, memory_usage=%d bytes, io_pressure=%.2f%%, inodes_usage=%d, disk_read=%d bytes, disk_write=%d bytes",
		containerStats.CPUPressure, containerStats.CPUUsage, containerStats.CPUThrottled, containerStats.MemoryUsage, containerStats.IOPressure, containerStats.InodesUsage, containerStats.DiskReadBytes, containerStats.DiskWriteBytes)

	// Gather metrics from registry
	return s.registry.Gather()
}

// Handler returns an HTTP handler for the /metrics endpoint
func (s *Server) Handler() http.Handler {
	return promhttp.HandlerFor(s, promhttp.HandlerOpts{})
}
