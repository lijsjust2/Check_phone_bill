//go:build !windows

package mobile

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// processAlive 检查进程是否存活（Unix 实现：信号 0 探测）
func processAlive(pid int) bool {
	return syscall.Kill(pid, syscall.Signal(0)) == nil
}

// killProcessTree 终止进程及其全部子孙进程（Chrome 的 crashpad 等子进程会
// 继承 user-data 文件句柄，只杀主进程会导致文件锁残留）
func killProcessTree(pid int) {
	children := map[int][]int{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		// 无 /proc（非 Linux），退化为单进程 kill
		killOne(pid)
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if pp := parentPid(p); pp > 0 {
			children[pp] = append(children[pp], p)
		}
	}

	var toKill []int
	var dfs func(int)
	dfs = func(p int) {
		toKill = append(toKill, p)
		for _, c := range children[p] {
			dfs(c)
		}
	}
	dfs(pid)
	for _, p := range toKill {
		killOne(p)
	}
}

func killOne(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

// parentPid 读取 /proc/<pid>/status 的 PPid
func parentPid(pid int) int {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "PPid:") {
			v, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "PPid:")))
			if err != nil {
				return 0
			}
			return v
		}
	}
	return 0
}
