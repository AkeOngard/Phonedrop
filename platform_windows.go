//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// freeSpace คืนพื้นที่ว่างของไดรฟ์ที่โฟลเดอร์ dir อยู่ (ไบต์)
func freeSpace(dir string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var free uint64
	err = windows.GetDiskFreeSpaceEx(p, &free, nil, nil)
	return free, err
}

// markFromNetwork ติดป้าย Mark-of-the-Web (Zone.Identifier) ให้ไฟล์ที่รับมา
// Windows จะเตือนก่อนรันไฟล์ .exe และเปิดเอกสาร Office ในโหมดป้องกัน
// (ใช้ได้กับ NTFS เท่านั้น — ไดรฟ์ FAT/exFAT จะข้ามไปเงียบ ๆ)
func markFromNetwork(file string) {
	os.WriteFile(file+":Zone.Identifier", []byte("[ZoneTransfer]\r\nZoneId=3\r\n"), 0o644)
}
