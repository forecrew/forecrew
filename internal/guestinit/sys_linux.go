package guestinit

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// machine is the system of a real guest.
type machine struct{}

// mountProc mounts /proc, the first thing: it is how the module loader is
// turned off.
func (machine) mountProc() error {
	if err := os.MkdirAll("/proc", 0o555); err != nil { // #nosec G301 -- a standard directory
		return err
	}
	err := syscall.Mount("proc", "/proc", "proc", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, "")
	if err != nil && !errors.Is(err, syscall.EBUSY) {
		return fmt.Errorf("/proc: %w", err)
	}
	return nil
}

// setClock sets the wall clock. Firecracker emulates no real-time clock, and
// certificates are checked against it.
func (machine) setClock(t time.Time) error {
	tv := syscall.NsecToTimeval(t.UnixNano())
	return syscall.Settimeofday(&tv)
}

// mountAll mounts the rest of what a Linux guest needs. The kernel mounted the
// root; its image may not have the directories.
func (machine) mountAll() error {
	for _, m := range []struct {
		fs, target string
		flags      uintptr
		data       string
	}{
		{"sysfs", "/sys", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, ""},
		{"devtmpfs", "/dev", syscall.MS_NOSUID, "mode=0755"},
		{"devpts", "/dev/pts", syscall.MS_NOSUID | syscall.MS_NOEXEC, "gid=5,mode=620,ptmxmode=666"},
		{"tmpfs", "/dev/shm", syscall.MS_NOSUID | syscall.MS_NODEV, ""},
		{"tmpfs", "/run", syscall.MS_NOSUID | syscall.MS_NODEV, "mode=0755"},
		{"tmpfs", "/tmp", syscall.MS_NOSUID | syscall.MS_NODEV, "mode=1777"},
		{"cgroup2", "/sys/fs/cgroup", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, ""},
	} {
		if err := os.MkdirAll(m.target, 0o755); err != nil { // #nosec G301 -- standard directories
			return err
		}
		err := syscall.Mount(m.fs, m.target, m.fs, m.flags, m.data)
		if err != nil && !errors.Is(err, syscall.EBUSY) { // devtmpfs may be mounted by the kernel
			return fmt.Errorf("%s: %w", m.target, err)
		}
	}
	_ = syscall.Sethostname([]byte("forecrew"))
	return nil
}

// loadModules loads kernel modules the provider named on the command line. The
// image's own list would be the repo's code in the guest kernel.
func (machine) loadModules(paths []string) error {
	var failed []string
	for _, p := range paths {
		if err := finitModule(p); err != nil {
			failed = append(failed, p+": "+err.Error())
		}
	}
	if len(failed) > 0 {
		return errors.New("load modules: " + strings.Join(failed, "; "))
	}
	return nil
}

func finitModule(path string) error {
	f, err := os.Open(path) // #nosec G304 -- the provider's command line
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := unix.FinitModule(int(f.Fd()), "", 0); err != nil && !errors.Is(err, syscall.EEXIST) { // #nosec G115 -- a file descriptor
		return err
	}
	return nil
}

// ext4ResizeFS is EXT4_IOC_RESIZE_FS, _IOW('f', 16, __u64).
const ext4ResizeFS = 0x40086610

// growRoot makes the root file system fill its disk. The provider sized the
// disk file; the guest grows the file system, because the file system is the
// image's to shape, and the host's tools should not parse it.
func (machine) growRoot(device string) error {
	dev, err := os.Open(device) // #nosec G304 -- the provider's command line
	if err != nil {
		return err
	}
	defer func() { _ = dev.Close() }()
	size, err := dev.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err != nil {
		return err
	}
	root, err := os.Open("/")
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	blocks := uint64(size) / uint64(st.Bsize)                                                                             // #nosec G115 -- both are positive
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, root.Fd(), ext4ResizeFS, uintptr(unsafe.Pointer(&blocks))); e != 0 { // #nosec G103 -- a system call
		return e
	}
	return nil
}

// mountCache mounts the read-only cache disk.
func (machine) mountCache(device, target string) error {
	if err := os.MkdirAll(target, 0o755); err != nil { // #nosec G301 -- every job reads it
		return err
	}
	return syscall.Mount(device, target, "ext4", syscall.MS_RDONLY|syscall.MS_NOSUID|syscall.MS_NODEV, "")
}

// wait reaps every child, as PID 1 must, until pid ends.
func (machine) wait(pid int) int {
	for {
		var ws syscall.WaitStatus
		got, err := syscall.Wait4(-1, &ws, 0, nil)
		switch {
		case errors.Is(err, syscall.EINTR):
		case err != nil:
			return -1
		case got == pid:
			return ws.ExitStatus()
		}
	}
}

// halt ends the machine. Firecracker has no power management and ends on a
// reset; Cloud Hypervisor restarts on one and ends on a power off, so the
// provider says which (forecrew.halt=reboot or poweroff).
func (machine) halt(how string) error {
	syscall.Sync()
	cmd := syscall.LINUX_REBOOT_CMD_POWER_OFF
	if how == "reboot" {
		cmd = syscall.LINUX_REBOOT_CMD_RESTART
	}
	return syscall.Reboot(cmd)
}
