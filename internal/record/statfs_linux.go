package record

// 磁盘空间检查（Linux 专用，其他平台返回错误由调用方忽略）。

import "syscall"

type syscallStatfs = syscall.Statfs_t

func statfs(path string, st *syscallStatfs) error {
	return syscall.Statfs(path, st)
}
