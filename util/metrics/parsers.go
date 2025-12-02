package metrics

import (
	"bufio"
	"strconv"
	"strings"

	"github.com/moby/buildkit/util/bklog"
)

// parseCPUUsage parses cpu.stat and extracts usage_usec
// Converts microseconds to milliseconds
func parseCPUUsage(data []byte) uint64 {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))

	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}

		if fields[0] == "usage_usec" {
			value, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				bklog.L.Errorf("error parsing usage_usec: %v", err)
				return 0
			}
			// Convert microseconds to milliseconds
			return value / 1000
		}
	}

	bklog.L.Errorf("usage_usec not found in cpu.stat")
	return 0
}

// parseCPUThrottled parses cpu.stat and extracts throttled_usec
// Converts microseconds to milliseconds
func parseCPUThrottled(data []byte) uint64 {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))

	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}

		if fields[0] == "throttled_usec" {
			value, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				bklog.L.Errorf("error parsing throttled_usec: %v", err)
				return 0
			}
			// Convert microseconds to milliseconds
			return value / 1000
		}
	}

	bklog.L.Errorf("throttled_usec not found in cpu.stat")
	return 0
}

// parseMemoryStat parses memory.stat format
// Returns working set memory (anon + kernel) which cannot be reclaimed
func parseMemoryStat(data []byte) uint64 {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	var anonMem, kernelMem uint64

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
		case "anon":
			anonMem = value
		case "kernel":
			kernelMem = value
		}
	}

	if anonMem == 0 && kernelMem == 0 {
		bklog.L.Errorf("anon and kernel fields not found in memory.stat")
		return 0
	}

	// Return working set: anon + kernel (non-reclaimable memory)
	return anonMem + kernelMem
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

// parseDiskIOStats parses io.stat and extracts metrics for the specified device
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
