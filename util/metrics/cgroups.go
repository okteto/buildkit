package metrics

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"syscall"

	"github.com/moby/buildkit/util/bklog"
	"k8s.io/apimachinery/pkg/api/resource"
)

// CgroupConfig holds the cgroup paths determined at startup
type CgroupConfig struct {
	cgroupVersion      int    // 1 for cgroup v1, 2 for cgroup v2
	daemonPath         string // Path to daemon cgroup (init in rootful, root in rootless) - v2 only
	containerPath      string // Path to container cgroup (buildkit in rootful, empty in rootless) - v2 only
	cpuPath            string // Path to CPU controller - v1 only
	memoryPath         string // Path to memory controller - v1 only
	blkioPath          string // Path to blkio controller - v1 only
	containerCPUPath   string // Path to container CPU cgroup - v1 only
	containerMemPath   string // Path to container memory cgroup - v1 only
	containerBlkioPath string // Path to container blkio cgroup - v1 only
	buildkitPath       string // Path to buildkit storage (/var/lib/buildkit or /home/user/.local/share/buildkit)
	storageDevice      string // Device major:minor for buildkit storage
}

// InitCgroupConfig detects the cgroup mode and initializes paths
func InitCgroupConfig() (*CgroupConfig, error) {
	config := &CgroupConfig{}

	// Detect cgroup version by checking for cgroup.controllers file (cgroup v2 indicator)
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err == nil {
		// cgroup v2 (unified hierarchy)
		config.cgroupVersion = 2
		bklog.L.Info("Detected cgroup v2")

		// Check if we're in rootful mode by looking for /var/lib/buildkit
		if _, err := os.Stat("/var/lib/buildkit"); err == nil {
			// Rootful mode: daemon runs in init cgroup, containers in buildkit cgroup
			config.daemonPath = "/sys/fs/cgroup/init"
			config.containerPath = "/sys/fs/cgroup/buildkit"
			config.buildkitPath = "/var/lib/buildkit"
		} else {
			// Rootless mode: everything runs in root cgroup
			config.daemonPath = "/sys/fs/cgroup"
			config.containerPath = "" // Empty in rootless mode
			config.buildkitPath = "/home/user/.local/share/buildkit"
		}
	} else {
		// cgroup v1 (separate controller hierarchies)
		config.cgroupVersion = 1
		bklog.L.Info("Detected cgroup v1")

		// Set controller-specific paths
		config.cpuPath = "/sys/fs/cgroup/cpu"
		config.memoryPath = "/sys/fs/cgroup/memory"
		config.blkioPath = "/sys/fs/cgroup/blkio"

		// Check if running as root by checking for /var/lib/buildkit
		if _, err := os.Stat("/var/lib/buildkit"); err == nil {
			// Rootful mode
			config.containerCPUPath = "/sys/fs/cgroup/cpu/buildkit"
			config.containerMemPath = "/sys/fs/cgroup/memory/buildkit"
			config.containerBlkioPath = "/sys/fs/cgroup/blkio/buildkit"
			config.buildkitPath = "/var/lib/buildkit"
		} else {
			// Rootless mode
			config.containerCPUPath = ""
			config.containerMemPath = ""
			config.containerBlkioPath = ""
			config.buildkitPath = "/home/user/.local/share/buildkit"
		}
	}

	// Verify the buildkit path exists
	if _, err := os.Stat(config.buildkitPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("buildkit storage path not found: %s", config.buildkitPath)
	}

	// Determine the storage device for disk I/O stats
	var stat syscall.Stat_t
	err := syscall.Stat(config.buildkitPath, &stat)
	if err != nil {
		return nil, fmt.Errorf("error stat'ing %s: %w", config.buildkitPath, err)
	}
	major := (stat.Dev >> 8) & 0xff
	minor := stat.Dev & 0xff
	config.storageDevice = fmt.Sprintf("%d:%d", major, minor)

	return config, nil
}

// ContainerStats represents the container's resource usage
type ContainerStats struct {
	CPUUsage         uint64  // Total CPU time consumed in milliseconds
	CPUThrottled     uint64  // Total time spent throttled in milliseconds
	CPUPressure      float64 // Percentage of time with tasks waiting for CPU (avg10)
	CPUTotal         uint64  // Total CPU available in millicores
	MemoryUsage      uint64  // Memory usage in bytes
	MemoryTotal      uint64  // Total memory available in bytes
	IOPressure       float64 // Percentage of time with tasks waiting for I/O (avg10)
	InodesUsage      uint64  // Inodes used on storage volume
	InodesTotal      uint64  // Total inodes available on storage volume
	DiskReadBytes    uint64  // Total bytes read from storage
	DiskWriteBytes   uint64  // Total bytes written to storage
	DiskReadOps      uint64  // Total read operations
	DiskWriteOps     uint64  // Total write operations
	DiskDiscardBytes uint64  // Total bytes discarded (TRIM)
	DiskDiscardOps   uint64  // Total discard operations
}

// getContainerStats reads cgroup metrics for memory, inodes, and pressure stats
func getContainerStats(config *CgroupConfig) *ContainerStats {
	stats := &ContainerStats{}

	// collectCPUMetrics
	cpuPressure := getCPUPressureStats(config)
	stats.CPUPressure = cpuPressure
	cpuUsage := getCPUUsageStats(config)
	stats.CPUUsage = cpuUsage
	cpuThrottled := getCPUThrottledStats(config)
	stats.CPUThrottled = cpuThrottled
	cpuTotal := getCPUTotal()
	stats.CPUTotal = cpuTotal

	// collectMemoryMetrics
	if config.cgroupVersion == 2 {
		daemonMemory := getMemoryUsageFromPath(config.daemonPath, config)
		containerMemory := getMemoryUsageFromPath(config.containerPath, config)
		stats.MemoryUsage = daemonMemory + containerMemory
	} else {
		// cgroup v1
		daemonMemory := getMemoryUsageFromPath(config.memoryPath, config)
		containerMemory := getMemoryUsageFromPath(config.containerMemPath, config)
		stats.MemoryUsage = daemonMemory + containerMemory
	}
	memoryTotal := getMemoryTotal()
	stats.MemoryTotal = memoryTotal

	// collectIOMetrics
	ioPressure := getIOPressureStats(config)
	stats.IOPressure = ioPressure

	//collectInodeMetrics
	inodesUsage, inodesTotal := getInodeStats(config.buildkitPath)
	stats.InodesUsage = inodesUsage
	stats.InodesTotal = inodesTotal

	// collectDiskMetrics
	diskReadBytes, diskWriteBytes, diskReadOps, diskWriteOps, diskDiscardBytes, diskDiscardOps := getDiskIOStats(config)
	stats.DiskReadBytes = diskReadBytes
	stats.DiskWriteBytes = diskWriteBytes
	stats.DiskReadOps = diskReadOps
	stats.DiskWriteOps = diskWriteOps
	stats.DiskDiscardBytes = diskDiscardBytes
	stats.DiskDiscardOps = diskDiscardOps

	return stats
}

// getCPUUsageStats reads CPU usage stats
// Returns usage_millis summed from daemon and container cgroups
func getCPUUsageStats(config *CgroupConfig) uint64 {
	if config.cgroupVersion == 2 {
		daemonUsage := getCPUUsageFromPath(config.daemonPath, "/cpu.stat")
		containerUsage := getCPUUsageFromPath(config.containerPath, "/cpu.stat")
		return daemonUsage + containerUsage
	}
	// cgroup v1
	daemonUsage := getCPUUsageFromPath(config.cpuPath, "/cpuacct.usage")
	containerUsage := getCPUUsageFromPath(config.containerCPUPath, "/cpuacct.usage")
	return daemonUsage + containerUsage
}

// getCPUUsageFromPath reads CPU usage from a specific cgroup path
func getCPUUsageFromPath(cgroupPath, filename string) uint64 {
	if cgroupPath == "" {
		return 0
	}
	data, err := os.ReadFile(cgroupPath + filename)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			bklog.L.Errorf("error reading %s: %v", cgroupPath+filename, err)
		}
		return 0
	}
	return parseCPUUsage(data)
}

// getCPUThrottledStats reads CPU throttled stats
// Returns throttled_millis summed from daemon and container cgroups
func getCPUThrottledStats(config *CgroupConfig) uint64 {
	if config.cgroupVersion == 2 {
		daemonThrottled := getCPUThrottledFromPath(config.daemonPath)
		containerThrottled := getCPUThrottledFromPath(config.containerPath)
		return daemonThrottled + containerThrottled
	}
	// cgroup v1
	daemonThrottled := getCPUThrottledFromPath(config.cpuPath)
	containerThrottled := getCPUThrottledFromPath(config.containerCPUPath)
	return daemonThrottled + containerThrottled
}

// getCPUThrottledFromPath reads CPU throttled from a specific cgroup path
func getCPUThrottledFromPath(cgroupPath string) uint64 {
	if cgroupPath == "" {
		return 0
	}
	data, err := os.ReadFile(cgroupPath + "/cpu.stat")
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			bklog.L.Errorf("error reading cpu.stat: %v", err)
		}
		return 0
	}

	return parseCPUThrottled(data)
}

// getCPUPressureStats reads CPU pressure stall information (PSI)
// Returns the "some" avg10 value (percentage of time with tasks waiting for CPU)
// For v2: Returns the maximum pressure across daemon and container cgroups
// For v1: Returns system-wide pressure from /proc/pressure/cpu
func getCPUPressureStats(config *CgroupConfig) float64 {
	if config.cgroupVersion == 2 {
		daemonPressure := getCPUPressureFromPath(config.daemonPath + "/cpu.pressure")
		containerPressure := getCPUPressureFromPath(config.containerPath + "/cpu.pressure")
		if containerPressure > daemonPressure {
			return containerPressure
		}
		return daemonPressure
	}
	// cgroup v1: use system-wide PSI
	return getCPUPressureFromPath("/proc/pressure/cpu")
}

// getCPUPressureFromPath reads CPU pressure from a specific path
func getCPUPressureFromPath(path string) float64 {
	if path == "" {
		return 0
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			bklog.L.Errorf("error reading %s: %v", path, err)
		}
		return 0
	}
	return parsePressureStatsSome(data)
}

// getCPUTotal returns total CPU available in millicores
// Uses BUILDKIT_CPU_LIMIT env var if available, otherwise system CPU count
func getCPUTotal() uint64 {
	if cpuLimit := os.Getenv("BUILDKIT_CPU_LIMIT"); cpuLimit != "" {
		// Parse CPU limit using Kubernetes resource.Quantity
		quantity, err := resource.ParseQuantity(cpuLimit)
		if err != nil {
			bklog.L.Errorf("error parsing BUILDKIT_CPU_LIMIT: %v", err)
			return 0
		}
		// Convert to millicores
		return uint64(quantity.MilliValue())
	}

	// Fallback to system CPU count (convert to millicores: 1 CPU = 1000 millicores)
	return uint64(runtime.NumCPU()) * 1000
}

// getMemoryUsageFromPath reads memory RSS from a specific cgroup path
func getMemoryUsageFromPath(cgroupPath string, config *CgroupConfig) uint64 {
	if cgroupPath == "" {
		return 0
	}

	// Determine the actual path based on cgroup version
	var actualPath string
	if config.cgroupVersion == 2 {
		actualPath = cgroupPath + "/memory.stat"
	} else {
		// For cgroup v1, map the generic path to the memory controller path
		if cgroupPath == config.cpuPath {
			actualPath = config.memoryPath + "/memory.stat"
		} else if cgroupPath == config.containerCPUPath {
			actualPath = config.containerMemPath + "/memory.stat"
		} else {
			actualPath = cgroupPath + "/memory.stat"
		}
	}

	data, err := os.ReadFile(actualPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			bklog.L.Errorf("error reading memory.stat: %v", err)
		}
		return 0
	}
	return parseMemoryStat(data)
}

// getMemoryTotal returns total memory available in bytes
// Uses BUILDKIT_MEMORY_LIMIT env var if available, otherwise system memory
func getMemoryTotal() uint64 {
	if memLimit := os.Getenv("BUILDKIT_MEMORY_LIMIT"); memLimit != "" {
		// Parse memory limit using Kubernetes resource.Quantity
		quantity, err := resource.ParseQuantity(memLimit)
		if err != nil {
			bklog.L.Errorf("error parsing BUILDKIT_MEMORY_LIMIT: %v", err)
			return 0
		}
		// Convert to bytes
		return uint64(quantity.Value())
	}

	// Fallback to system memory
	var si syscall.Sysinfo_t
	err := syscall.Sysinfo(&si)
	if err != nil {
		bklog.L.Errorf("error reading system memory: %v", err)
		return 0
	}
	return si.Totalram * uint64(si.Unit)
}

// getIOPressureStats reads I/O pressure stall information (PSI)
// Returns the "some" avg10 value (percentage of time with tasks waiting for I/O)
// For v2: Returns the maximum pressure across daemon and container cgroups
// For v1: Returns system-wide pressure from /proc/pressure/io
func getIOPressureStats(config *CgroupConfig) float64 {
	if config.cgroupVersion == 2 {
		daemonPressure := getIOPressureFromPath(config.daemonPath + "/io.pressure")
		containerPressure := getIOPressureFromPath(config.containerPath + "/io.pressure")

		// Return maximum pressure (worst case)
		if containerPressure > daemonPressure {
			return containerPressure
		}
		return daemonPressure
	}
	// cgroup v1: use system-wide PSI
	return getIOPressureFromPath("/proc/pressure/io")
}

// getIOPressureFromPath reads I/O pressure from a specific path
// Returns the "some" avg10 value
func getIOPressureFromPath(path string) float64 {
	if path == "" {
		return 0
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			bklog.L.Errorf("error reading %s: %v", path, err)
		}
		return 0
	}
	return parsePressureStatsSome(data)
}

// getInodeStats reads inode usage from the buildkit storage volume
// Returns (usage, total)
func getInodeStats(buildkitPath string) (uint64, uint64) {
	var stat syscall.Statfs_t
	err := syscall.Statfs(buildkitPath, &stat)
	if err != nil {
		bklog.L.Errorf("error reading filesystem stats for %s: %v", buildkitPath, err)
		return 0, 0
	}
	// stat.Files = total inodes
	// stat.Ffree = free inodes
	// used = total - free
	return stat.Files - stat.Ffree, stat.Files
}

// getDiskIOStats reads disk I/O stats from the buildkit storage device
// Returns (readBytes, writeBytes, readOps, writeOps, discardBytes, discardOps, error)
// Sums values from daemon and container cgroups for the buildkit storage device
func getDiskIOStats(config *CgroupConfig) (uint64, uint64, uint64, uint64, uint64, uint64) {
	if config.cgroupVersion == 2 {
		daemonRead, daemonWrite, daemonReadOps, daemonWriteOps, daemonDiscard, daemonDiscardOps := getDiskIOFromPath(config.daemonPath, config.storageDevice, config.cgroupVersion)
		containerRead, containerWrite, containerReadOps, containerWriteOps, containerDiscard, containerDiscardOps := getDiskIOFromPath(config.containerPath, config.storageDevice, config.cgroupVersion)
		return daemonRead + containerRead,
			daemonWrite + containerWrite,
			daemonReadOps + containerReadOps,
			daemonWriteOps + containerWriteOps,
			daemonDiscard + containerDiscard,
			daemonDiscardOps + containerDiscardOps
	}
	// cgroup v1
	daemonRead, daemonWrite, daemonReadOps, daemonWriteOps, daemonDiscard, daemonDiscardOps := getDiskIOFromPath(config.blkioPath, config.storageDevice, config.cgroupVersion)
	containerRead, containerWrite, containerReadOps, containerWriteOps, containerDiscard, containerDiscardOps := getDiskIOFromPath(config.containerBlkioPath, config.storageDevice, config.cgroupVersion)
	return daemonRead + containerRead,
		daemonWrite + containerWrite,
		daemonReadOps + containerReadOps,
		daemonWriteOps + containerWriteOps,
		daemonDiscard + containerDiscard,
		daemonDiscardOps + containerDiscardOps
}

// getDiskIOFromPath reads disk I/O stats from a specific cgroup path for a specific device
func getDiskIOFromPath(cgroupPath, deviceMajorMinor string, cgroupVersion int) (uint64, uint64, uint64, uint64, uint64, uint64) {
	if cgroupPath == "" {
		return 0, 0, 0, 0, 0, 0
	}

	if cgroupVersion == 2 {
		// cgroup v2: read io.stat
		data, err := os.ReadFile(cgroupPath + "/io.stat")
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				bklog.L.Errorf("error while reading io.stat: %v", err)
			}
			return 0, 0, 0, 0, 0, 0
		}
		return parseDiskIOStats(data, deviceMajorMinor)
	}

	// cgroup v1: read blkio.throttle.io_service_bytes
	data, err := os.ReadFile(cgroupPath + "/blkio.throttle.io_service_bytes")
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			bklog.L.Errorf("error while reading blkio.throttle.io_service_bytes: %v", err)
		}
		return 0, 0, 0, 0, 0, 0
	}
	return parseBlkioStats(data, deviceMajorMinor)
}
