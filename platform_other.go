//go:build !windows

package main

import "golang.org/x/sys/unix"

// freeSpace คืนพื้นที่ว่างของดิสก์ที่โฟลเดอร์ dir อยู่ (ไบต์)
func freeSpace(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// markFromNetwork: Linux/macOS ไม่ให้สิทธิ์ execute กับไฟล์ที่สร้างใหม่อยู่แล้ว จึงไม่ต้องทำอะไรเพิ่ม
func markFromNetwork(file string) {}
