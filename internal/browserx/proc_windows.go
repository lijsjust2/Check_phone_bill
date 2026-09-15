//go:build windows

package browserx

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

const stillActive = 259 // windows.STILL_ACTIVE

// processAlive 检查进程是否存活（Windows 实现）
// 注意：无法查询（权限不足等）时按「存活」处理——宁可多杀一次（无害），
// 也不要误判已退出导致进程残留锁住 user-data 目录
func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return true // 无法打开（权限受限/沙箱环境）→ 按存活处理
	}
	defer windows.CloseHandle(h)
	var ec uint32
	if err := windows.GetExitCodeProcess(h, &ec); err != nil {
		return true
	}
	return ec == stillActive
}

// killProcessTree 终止进程及其全部子孙进程（Chrome 的 crashpad 等子进程会
// 继承 user-data 文件句柄，只杀主进程会导致文件锁残留）
func killProcessTree(pid int) {
	// 枚举系统进程快照，建立 父PID → 子PID列表 映射
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return
	}
	defer windows.CloseHandle(snap)

	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	children := map[uint32][]uint32{}
	if err := windows.Process32First(snap, &pe); err == nil {
		for {
			children[pe.ParentProcessID] = append(children[pe.ParentProcessID], pe.ProcessID)
			if err := windows.Process32Next(snap, &pe); err != nil {
				break
			}
		}
	}

	// 深度优先收集整个子树
	var toKill []uint32
	var dfs func(uint32)
	dfs = func(p uint32) {
		toKill = append(toKill, p)
		for _, c := range children[p] {
			dfs(c)
		}
	}
	dfs(uint32(pid))

	for _, p := range toKill {
		if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, p); err == nil {
			_ = windows.TerminateProcess(h, 1)
			_ = windows.CloseHandle(h)
		}
	}
}
