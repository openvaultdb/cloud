package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadCgroupV1MetricsAndCapacity(t *testing.T) {
	root := t.TempDir()
	writeCgroupFixture(t, root, map[string]string{
		"memory/memory.usage_in_bytes":     "4534272\n",
		"memory/memory.max_usage_in_bytes": "19173376\n",
		"memory/memory.limit_in_bytes":     "536870912\n",
		"memory/memory.failcnt":            "0\n",
		"memory/memory.stat":               "cache 4096\nrss 1024\n",
		"memory/memory.oom_control":        "oom_kill_disable 0\nunder_oom 0\noom_kill 0\n",
		"cpu,cpuacct/cpu.cfs_quota_us":     "98600\n",
		"cpu,cpuacct/cpu.cfs_period_us":    "100000\n",
		"cpu,cpuacct/cpuacct.usage":        "4534272\n",
	})

	metrics, err := readCgroupMetrics(root)
	if err != nil {
		t.Fatal(err)
	}
	if metrics["cgroup.version"] != "v1" || metrics["memory.current"] != uint64(4534272) || metrics["memory.peak"] != uint64(19173376) || metrics["memory.max"] != uint64(536870912) {
		t.Fatalf("normalized v1 memory metrics = %#v", metrics)
	}
	if metrics["cpu.max"] != "98600 100000" || metrics["cpu.usage_usec"] != uint64(4534) {
		t.Fatalf("normalized v1 CPU metrics = %#v", metrics)
	}
	if stats := metrics["memory.stat"].(map[string]uint64); stats["cache"] != 4096 || stats["rss"] != 1024 {
		t.Fatalf("v1 memory.stat was not preserved: %#v", stats)
	}
	if err := validateCgroupCapacity(metrics); err != nil {
		t.Fatalf("observed Cloud Run cgroup v1 values rejected: %v", err)
	}
}

func TestReadCgroupV2MetricsAndCapacity(t *testing.T) {
	root := t.TempDir()
	writeCgroupFixture(t, root, map[string]string{
		"memory.current": "4534272\n",
		"memory.peak":    "19173376\n",
		"memory.max":     "536870912\n",
		"memory.events":  "low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\n",
		"memory.stat":    "anon 1024\nfile 4096\n",
		"cpu.max":        "98600 100000\n",
		"cpu.stat":       "usage_usec 4534\nuser_usec 3200\nsystem_usec 1334\n",
	})

	metrics, err := readCgroupMetrics(root)
	if err != nil {
		t.Fatal(err)
	}
	if metrics["cgroup.version"] != "v2" || metrics["cpu.usage_usec"] != uint64(4534) {
		t.Fatalf("normalized v2 metrics = %#v", metrics)
	}
	if stats := metrics["memory.stat"].(map[string]uint64); stats["anon"] != 1024 || stats["file"] != 4096 {
		t.Fatalf("v2 memory.stat was not preserved: %#v", stats)
	}
	if err := validateCgroupCapacity(metrics); err != nil {
		t.Fatalf("valid cgroup v2 metrics rejected: %v", err)
	}
}

func TestReadCgroupV1MetricsFailsClosedOnMissingOrUnsafeValues(t *testing.T) {
	base := map[string]string{
		"memory/memory.usage_in_bytes":     "4534272\n",
		"memory/memory.max_usage_in_bytes": "19173376\n",
		"memory/memory.limit_in_bytes":     "536870912\n",
		"memory/memory.failcnt":            "0\n",
		"memory/memory.stat":               "cache 4096\nrss 1024\n",
		"memory/memory.oom_control":        "oom_kill_disable 0\nunder_oom 0\noom_kill 0\n",
		"cpu,cpuacct/cpu.cfs_quota_us":     "98600\n",
		"cpu,cpuacct/cpu.cfs_period_us":    "100000\n",
		"cpu,cpuacct/cpuacct.usage":        "4534272\n",
	}
	for _, test := range []struct {
		name   string
		change func(map[string]string)
	}{
		{name: "missing OOM kill counter", change: func(files map[string]string) { files["memory/memory.oom_control"] = "under_oom 0\n" }},
		{name: "unlimited CPU", change: func(files map[string]string) { files["cpu,cpuacct/cpu.cfs_quota_us"] = "-1\n" }},
		{name: "CPU over one core", change: func(files map[string]string) { files["cpu,cpuacct/cpu.cfs_quota_us"] = "100001\n" }},
		{name: "missing memory pressure counter", change: func(files map[string]string) { delete(files, "memory/memory.failcnt") }},
		{name: "missing CPU accounting", change: func(files map[string]string) { delete(files, "cpu,cpuacct/cpuacct.usage") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			files := make(map[string]string, len(base))
			for name, contents := range base {
				files[name] = contents
			}
			test.change(files)
			root := t.TempDir()
			writeCgroupFixture(t, root, files)
			if _, err := readCgroupMetrics(root); err == nil {
				t.Fatal("unsafe or incomplete cgroup v1 metrics were accepted")
			}
		})
	}
}

func TestValidateCgroupCapacityRejectsPressureAndOutOfEnvelope(t *testing.T) {
	base := map[string]any{
		"memory.current": uint64(4534272),
		"memory.peak":    uint64(19173376),
		"memory.max":     uint64(512 << 20),
		"cpu.max":        "98600 100000",
		"memory.events":  map[string]uint64{"max": 0, "oom": 0, "oom_kill": 0},
	}
	for _, test := range []struct {
		name   string
		change func(map[string]any)
	}{
		{name: "wrong memory limit", change: func(m map[string]any) { m["memory.max"] = uint64(511 << 20) }},
		{name: "peak over budget", change: func(m map[string]any) { m["memory.peak"] = uint64(435<<20) + 1 }},
		{name: "memory pressure", change: func(m map[string]any) { m["memory.events"].(map[string]uint64)["max"] = 1 }},
		{name: "unlimited CPU", change: func(m map[string]any) { m["cpu.max"] = "max 100000" }},
		{name: "CPU over configured one CPU", change: func(m map[string]any) { m["cpu.max"] = "100001 100000" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			metrics := cloneMetrics(base)
			test.change(metrics)
			if err := validateCgroupCapacity(metrics); err == nil {
				t.Fatal("invalid capacity metrics were accepted")
			}
		})
	}
}

func writeCgroupFixture(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, contents := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func cloneMetrics(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		if events, ok := value.(map[string]uint64); ok {
			copy := make(map[string]uint64, len(events))
			for name, count := range events {
				copy[name] = count
			}
			result[key] = copy
		} else {
			result[key] = value
		}
	}
	return result
}
