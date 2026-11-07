package fleetagent

import (
	"syscall"
	"unsafe"
)

var kernel32 = syscall.NewLazyDLL("kernel32.dll")

// memoryStatus is Windows' MEMORYSTATUSEX.
type memoryStatus struct {
	length, load                       uint32
	total, avail, pageTotal, pageAvail uint64
	virtTotal, virtAvail, virtExtended uint64
}

// MemoryMB says how much memory this machine has.
func MemoryMB() (int, error) {
	m := memoryStatus{}
	m.length = uint32(unsafe.Sizeof(m))
	r, _, err := kernel32.NewProc("GlobalMemoryStatusEx").Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 {
		return 0, err
	}
	return int(m.total >> 20), nil
}

// DiskFreeGB says how much of the file system of path is free.
func DiskFreeGB(path string) (int, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var free uint64
	r, _, err := kernel32.NewProc("GetDiskFreeSpaceExW").Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&free)), 0, 0)
	if r == 0 {
		return 0, err
	}
	return int(free >> 30), nil
}
