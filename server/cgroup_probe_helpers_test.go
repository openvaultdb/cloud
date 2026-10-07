package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// readCgroupMetrics returns the memory and CPU measurements used by the
// production-capacity probes, normalized to the cgroup v2 file names. Cloud
// Run may expose a v1 hierarchy even when a cgroup v2 mount is also present.
func readCgroupMetrics(root string) (map[string]any, error) {
	if _, err := os.Stat(filepath.Join(root, "memory.max")); err == nil {
		return readCgroupV2Metrics(root)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspect cgroup memory.max: %w", err)
	}
	return readCgroupV1Metrics(root)
}

func readCgroupV2Metrics(root string) (map[string]any, error) {
	current, err := readUint(filepath.Join(root, "memory.current"))
	if err != nil {
		return nil, err
	}
	peak, err := readUint(filepath.Join(root, "memory.peak"))
	if err != nil {
		return nil, err
	}
	limit, err := readUint(filepath.Join(root, "memory.max"))
	if err != nil {
		return nil, err
	}
	cpuMax, err := readText(filepath.Join(root, "cpu.max"))
	if err != nil {
		return nil, err
	}
	events, err := readKeyValues(filepath.Join(root, "memory.events"))
	if err != nil {
		return nil, err
	}
	for _, required := range []string{"max", "oom", "oom_kill"} {
		if _, ok := events[required]; !ok {
			return nil, fmt.Errorf("cgroup memory.events lacks %s", required)
		}
	}
	cpuStats, err := readKeyValues(filepath.Join(root, "cpu.stat"))
	if err != nil {
		return nil, err
	}
	cpuUsage, ok := cpuStats["usage_usec"]
	if !ok {
		return nil, fmt.Errorf("cgroup cpu.stat lacks usage_usec")
	}
	memoryStat, err := readKeyValues(filepath.Join(root, "memory.stat"))
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"memory.current": current,
		"memory.peak":    peak,
		"memory.max":     limit,
		"cpu.max":        cpuMax,
		"cpu.usage_usec": cpuUsage,
		"memory.stat":    memoryStat,
		"memory.events":  events,
		"cgroup.version": "v2",
	}, nil
}

func readCgroupV1Metrics(root string) (map[string]any, error) {
	memoryDir := filepath.Join(root, "memory")
	cpuDir := ""
	for _, candidate := range []string{"cpu,cpuacct", "cpu"} {
		if _, err := os.Stat(filepath.Join(root, candidate, "cpu.cfs_quota_us")); err == nil {
			cpuDir = filepath.Join(root, candidate)
			break
		}
	}
	if cpuDir == "" {
		return nil, fmt.Errorf("cgroup v1 cpu quota files not found")
	}
	cpuAccountingDir := ""
	for _, candidate := range []string{"cpu,cpuacct", "cpuacct", "cpu"} {
		if _, err := os.Stat(filepath.Join(root, candidate, "cpuacct.usage")); err == nil {
			cpuAccountingDir = filepath.Join(root, candidate)
			break
		}
	}
	if cpuAccountingDir == "" {
		return nil, fmt.Errorf("cgroup v1 CPU accounting file not found")
	}
	current, err := readUint(filepath.Join(memoryDir, "memory.usage_in_bytes"))
	if err != nil {
		return nil, err
	}
	peak, err := readUint(filepath.Join(memoryDir, "memory.max_usage_in_bytes"))
	if err != nil {
		return nil, err
	}
	limit, err := readUint(filepath.Join(memoryDir, "memory.limit_in_bytes"))
	if err != nil {
		return nil, err
	}
	failCount, err := readUint(filepath.Join(memoryDir, "memory.failcnt"))
	if err != nil {
		return nil, err
	}
	quotaText, err := readText(filepath.Join(cpuDir, "cpu.cfs_quota_us"))
	if err != nil {
		return nil, err
	}
	quota, err := strconv.ParseInt(quotaText, 10, 64)
	if err != nil || quota <= 0 {
		return nil, fmt.Errorf("cgroup v1 CPU quota is invalid or unlimited")
	}
	period, err := readUint(filepath.Join(cpuDir, "cpu.cfs_period_us"))
	if err != nil || period == 0 {
		return nil, fmt.Errorf("cgroup v1 CPU period is invalid")
	}
	if uint64(quota) > period {
		return nil, fmt.Errorf("cgroup v1 CPU quota exceeds one CPU")
	}
	oomControl, err := readKeyValues(filepath.Join(memoryDir, "memory.oom_control"))
	if err != nil {
		return nil, err
	}
	oomKill, ok := oomControl["oom_kill"]
	if !ok {
		return nil, fmt.Errorf("cgroup v1 memory.oom_control lacks oom_kill")
	}
	cpuUsageNS, err := readUint(filepath.Join(cpuAccountingDir, "cpuacct.usage"))
	if err != nil {
		return nil, err
	}
	underOOM, ok := oomControl["under_oom"]
	if !ok {
		return nil, fmt.Errorf("cgroup v1 memory.oom_control lacks under_oom")
	}
	memoryStat, err := readKeyValues(filepath.Join(memoryDir, "memory.stat"))
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"memory.current": current,
		"memory.peak":    peak,
		"memory.max":     limit,
		"cpu.max":        fmt.Sprintf("%d %d", quota, period),
		"cpu.usage_usec": cpuUsageNS / 1_000,
		"memory.stat":    memoryStat,
		"memory.events": map[string]uint64{
			// v1 failcnt counts attempted charges at the hard limit; it is the
			// closest equivalent to v2's max event.
			"max":      failCount,
			"oom":      underOOM,
			"oom_kill": oomKill,
		},
		"cgroup.version": "v1",
	}, nil
}

func validateCgroupCapacity(metrics map[string]any) error {
	limit, ok := metrics["memory.max"].(uint64)
	if !ok || limit != 512<<20 {
		return fmt.Errorf("memory limit is not 512MiB")
	}
	peak, ok := metrics["memory.peak"].(uint64)
	if !ok || peak > 435<<20 {
		return fmt.Errorf("memory peak exceeds 435MiB")
	}
	events, ok := metrics["memory.events"].(map[string]uint64)
	if !ok {
		return fmt.Errorf("memory pressure counters are unavailable")
	}
	for _, name := range []string{"max", "oom", "oom_kill"} {
		value, exists := events[name]
		if !exists || value != 0 {
			return fmt.Errorf("memory pressure counter %s is nonzero or unavailable", name)
		}
	}
	cpu, ok := metrics["cpu.max"].(string)
	if !ok {
		return fmt.Errorf("CPU quota is unavailable")
	}
	fields := strings.Fields(cpu)
	if len(fields) != 2 {
		return fmt.Errorf("CPU quota is invalid")
	}
	quota, quotaErr := strconv.ParseUint(fields[0], 10, 64)
	period, periodErr := strconv.ParseUint(fields[1], 10, 64)
	if quotaErr != nil || periodErr != nil || quota == 0 || period == 0 || quota > period {
		return fmt.Errorf("CPU quota is not positive and at most one CPU")
	}
	return nil
}

func readUint(path string) (uint64, error) {
	value, err := readText(path)
	if err != nil {
		return 0, err
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid cgroup metric %s", filepath.Base(path))
	}
	return parsed, nil
}

func readText(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read cgroup metric %s: %w", filepath.Base(path), err)
	}
	return strings.TrimSpace(string(data)), nil
}

func readKeyValues(path string) (map[string]uint64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read cgroup metric %s: %w", filepath.Base(path), err)
	}
	defer file.Close()
	values := map[string]uint64{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			return nil, fmt.Errorf("invalid cgroup metric %s", filepath.Base(path))
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid cgroup metric %s", filepath.Base(path))
		}
		if _, exists := values[fields[0]]; exists {
			return nil, fmt.Errorf("duplicate cgroup metric %s", fields[0])
		}
		values[fields[0]] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read cgroup metric %s: %w", filepath.Base(path), err)
	}
	return values, nil
}
