//go:build !linux

package main

import "os"

// 非 Linux 平台只用于开发机上的单元测试与编译检查：接管逻辑本身依赖
// Unix socket 与 flock，真正运行环境只有飞牛 NAS（Linux）。

func fileInode(path string) (uint64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	// 用修改时间做退化的身份标记，保证测试里"两次观察是否同一个文件"仍可用。
	return uint64(info.ModTime().UnixNano()), nil
}

func tryLock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	return func() { f.Close() }, nil
}
