//go:build linux

package main

import (
	"os"
	"syscall"
)

// fileInode 返回路径对应文件的 inode 号。
//
// 接管过程中必须靠 inode 而不是路径判断"这个 socket 还是不是我挪走的那一个"：
// 路径会被 rename 覆盖，inode 才是内核层面唯一稳定的身份。
func fileInode(path string) (uint64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, os.ErrInvalid
	}
	return st.Ino, nil
}

// tryLock 对文件加独占 flock，返回释放函数。
//
// 用 flock 而不是"创建锁文件"：进程崩溃（包括 SIGKILL）时内核自动释放 flock，
// 而锁文件会留下来，把下一次启动永久挡在门外。
func tryLock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
