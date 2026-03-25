package metrics

import (
	"bufio"
	"strconv"
	"strings"

	"github.com/moby/buildkit/util/bklog"
)

// parseCPUUsage parses cpu.stat (v2) or cpuacct.usage (v1) and extracts usage
// For v2: extracts usage_usec and converts microseconds to milliseconds
// For v1: expects nanoseconds and converts to milliseconds
func parseCPUUsage(data []byte) uint64 {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))

	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)

		// cgroup v2 format: "usage_usec 123456"
		if len(fields) == 2 && fields[0] == "usage_usec" {
			value, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				bklog.L.Errorf("error parsing usage_usec: %v", err)
				return 0
			}
			// Convert microseconds to milliseconds
			return value / 1000
		}

		// cgroup v1 format: single number in nanoseconds
		if len(fields) == 1 {
			value, err := strconv.ParseUint(fields[0], 10, 64)
			if err != nil {
				bklog.L.Errorf("error parsing cpuacct.usage: %v", err)
				return 0
			}
			// Convert nanoseconds to milliseconds
			return value / 1000000
		}
	}

	bklog.L.Errorf("CPU usage not found in expected format")
	return 0
}

// parseCPUThrottled parses cpu.stat and extracts throttled time
// For v2: extracts throttled_usec and converts microseconds to milliseconds
// For v1: extracts throttled_time and converts nanoseconds to milliseconds
func parseCPUThrottled(data []byte) uint64 {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))

	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}

		// cgroup v2 format: "throttled_usec 123456"
		if fields[0] == "throttled_usec" {
			value, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				bklog.L.Errorf("error parsing throttled_usec: %v", err)
				return 0
			}
			// Convert microseconds to milliseconds
			return value / 1000
		}

		// cgroup v1 format: "throttled_time 123456789"
		if fields[0] == "throttled_time" {
			value, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				bklog.L.Errorf("error parsing throttled_time: %v", err)
				return 0
			}
			// Convert nanoseconds to milliseconds
			return value / 1000000
		}
	}

	// Return 0 if not found (no throttling has occurred)
	return 0
}

// parseMemoryStat parses memory.stat format
// For v2: Returns working set memory (anon + kernel) which cannot be reclaimed
// For v1: Returns rss (resident set size)
func parseMemoryStat(data []byte) uint64 {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	var anonMem, kernelMem, rssMem uint64

	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}

		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}

		switch fields[0] {
		case "anon": // cgroup v2
			anonMem = value
		case "kernel": // cgroup v2
			kernelMem = value
		case "rss": // cgroup v1
			rssMem = value
		}
	}

	// cgroup v2: return anon + kernel
	if anonMem > 0 || kernelMem > 0 {
		return anonMem + kernelMem
	}

	// cgroup v1: return rss
	if rssMem > 0 {
		return rssMem
	}

	bklog.L.Errorf("memory fields not found in memory.stat")
	return 0
}

// parsePressureStatsSome parses pressure stall information and returns "some" avg10 value
// Format:
// some avg10=0.42 avg60=0.20 avg300=0.38 total=41569357084
// full avg10=0.18 avg60=0.08 avg300=0.21 total=32255254794
func parsePressureStatsSome(data []byte) float64 {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		// Look for the "some" line
		if fields[0] == "some" {
			// Parse avg10 value from "avg10=0.42"
			for _, field := range fields[1:] {
				if strings.HasPrefix(field, "avg10=") {
					valueStr := strings.TrimPrefix(field, "avg10=")
					value, err := strconv.ParseFloat(valueStr, 64)
					if err != nil {
						bklog.L.Errorf("error parsing avg10 value: %v", err)
						return 0
					}
					return value
				}
			}
		}
	}
	bklog.L.Errorf("some avg10 field not found in pressure stats")
	return 0
}

// parseDiskIOStats parses io.stat (cgroup v2) and extracts metrics for the specified device
// Format: "8:96 rbytes=123 wbytes=456 rios=10 wios=20 dbytes=0 dios=0"
func parseDiskIOStats(data []byte, deviceMajorMinor string) (uint64, uint64, uint64, uint64, uint64, uint64) {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))

	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		// Check if this line is for our device
		if fields[0] != deviceMajorMinor {
			continue
		}

		// Parse the key=value pairs
		var rbytes, wbytes, rios, wios, dbytes, dios uint64
		for _, field := range fields[1:] {
			parts := strings.Split(field, "=")
			if len(parts) != 2 {
				continue
			}

			value, err := strconv.ParseUint(parts[1], 10, 64)
			if err != nil {
				continue
			}

			switch parts[0] {
			case "rbytes":
				rbytes = value
			case "wbytes":
				wbytes = value
			case "rios":
				rios = value
			case "wios":
				wios = value
			case "dbytes":
				dbytes = value
			case "dios":
				dios = value
			}
		}

		return rbytes, wbytes, rios, wios, dbytes, dios
	}

	// Device not found in io.stat (could be no I/O yet)
	return 0, 0, 0, 0, 0, 0
}

// parseBlkioStats parses blkio.throttle.io_service_bytes (cgroup v1) and extracts metrics for the specified device
// Format: "259:3 Read 135168\n259:3 Write 90906624\n259:3 Discard 0\nTotal 91049984"
func parseBlkioStats(data []byte, deviceMajorMinor string) (uint64, uint64, uint64, uint64, uint64, uint64) {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	var rbytes, wbytes, dbytes uint64

	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}

		// Check if this line is for our device
		if fields[0] != deviceMajorMinor {
			continue
		}

		value, err := strconv.ParseUint(fields[2], 10, 64)
		if err != nil {
			continue
		}

		switch fields[1] {
		case "Read":
			rbytes = value
		case "Write":
			wbytes = value
		case "Discard":
			dbytes = value
		}
	}

	// cgroup v1 doesn't provide operation counts, only bytes
	return rbytes, wbytes, 0, 0, dbytes, 0
}
