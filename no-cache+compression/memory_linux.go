package main

import (
	"os"
	"strconv"
	"strings"
)

// physicalMemory is MemTotal from /proc/meminfo, in bytes.
func physicalMemory() uint64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}

// cgroupMemoryLimit is the tightest cgroup memory limit for this process.
// cgroup v2 memory.max is used when that file is present. Otherwise cgroup
// v1 memory.limit_in_bytes is used. A huge v1 unlimited sentinel is ignored.
// Zero means no numeric limit was found.
func cgroupMemoryLimit() uint64 {
	if n, present := cgroupV2Limit(); present {
		return n
	}
	return cgroupV1Limit()
}

func cgroupV2Limit() (uint64, bool) {
	path := selfCgroupV2Path()
	if path == "" {
		if n, ok := readCgroupLimit("/sys/fs/cgroup/memory.max"); ok {
			return n, true
		}
		// The file may exist and say "max" (unlimited). That still means the
		// v2 controller is present, so the v1 file is not consulted.
		if cgroupFilePresent("/sys/fs/cgroup/memory.max") {
			return 0, true
		}
		return 0, false
	}
	var smallest uint64
	foundNumeric := false
	present := false
	for {
		file := cgroupMemoryMaxFile(path)
		if cgroupFilePresent(file) {
			present = true
			if n, ok := readCgroupLimit(file); ok && (!foundNumeric || n < smallest) {
				smallest = n
				foundNumeric = true
			}
		}
		if path == "/" {
			break
		}
		parent := path[:strings.LastIndex(path, "/")]
		if parent == "" {
			parent = "/"
		}
		path = parent
	}
	if !present {
		return 0, false
	}
	if !foundNumeric {
		return 0, true
	}
	return smallest, true
}

func cgroupFilePresent(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// cgroupV1Limit reads memory.limit_in_bytes for this process's memory cgroup
// and its parents, including /sys/fs/cgroup/memory/memory.limit_in_bytes.
// The smallest numeric limit wins.
func cgroupV1Limit() uint64 {
	var smallest uint64
	found := false
	consider := func(path string) {
		n, ok := readCgroupLimit(path)
		if ok && (!found || n < smallest) {
			smallest = n
			found = true
		}
	}
	if dir := selfCgroupV1MemoryDir(); dir != "" {
		for {
			consider(dir + "/memory.limit_in_bytes")
			if dir == "/sys/fs/cgroup/memory" {
				break
			}
			parent := dir[:strings.LastIndex(dir, "/")]
			if len(parent) < len("/sys/fs/cgroup/memory") {
				break
			}
			dir = parent
		}
	}
	consider("/sys/fs/cgroup/memory/memory.limit_in_bytes")
	if !found {
		return 0
	}
	return smallest
}

func selfCgroupV2Path() string {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "0::")
		if !ok {
			continue
		}
		if rest == "" {
			return "/"
		}
		if !strings.HasPrefix(rest, "/") {
			rest = "/" + rest
		}
		if len(rest) > 1 {
			rest = strings.TrimRight(rest, "/")
		}
		return rest
	}
	return ""
}

func cgroupMemoryMaxFile(cgPath string) string {
	if cgPath == "/" {
		return "/sys/fs/cgroup/memory.max"
	}
	return "/sys/fs/cgroup" + cgPath + "/memory.max"
}

func selfCgroupV1MemoryDir() string {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(parts) != 3 || parts[1] == "" {
			continue
		}
		for _, ctrl := range strings.Split(parts[1], ",") {
			if ctrl != "memory" {
				continue
			}
			p := parts[2]
			if p == "" || p == "/" {
				return "/sys/fs/cgroup/memory"
			}
			if !strings.HasPrefix(p, "/") {
				p = "/" + p
			}
			return "/sys/fs/cgroup/memory" + strings.TrimRight(p, "/")
		}
	}
	return ""
}

// readCgroupLimit reads a numeric byte limit. "max" and the huge v1
// unlimited sentinel (anything at or above 1<<62) are not numeric limits.
func readCgroupLimit(path string) (uint64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(b))
	if s == "" || s == "max" {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || n == 0 || n >= 1<<62 {
		return 0, false
	}
	return n, true
}
