package microvm

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/images"
)

// The VMMs, as a shape's provider_config names them.
const (
	vmmFirecracker     = "firecracker"
	vmmCloudHypervisor = "cloud-hypervisor"
)

// File names of a machine, inside its directory.
const (
	fileKernel = "kernel"
	fileRoot   = "root.ext4"
	fileConfig = "config.img"
	fileCache  = "cache.ext4"
	fileVM     = "vm.json"
)

// Devices of a machine, in the order of its disks.
const (
	deviceRoot   = "/dev/vda"
	deviceConfig = "/dev/vdb"
	deviceCache  = "/dev/vdc"
)

// spec is a machine as a VMM starts it.
type spec struct {
	ID       string
	Dir      string // where its files are, on the host
	CPU      int
	MemoryMB int
	Tap, MAC string
	BootArgs string
	Cache    bool
	GPUs     []string
	UID, GID int
}

// vmm is one way of running a machine.
type vmm interface {
	// jailDir returns the machine's directory on the host.
	jailDir(id string) string
	// halt says how the guest must end for the VMM to exit.
	halt() string
	// bootArgs are what this VMM's guests need on their command line.
	bootArgs() string
	// command writes what the VMM reads and returns the process to start.
	command(s spec) (*exec.Cmd, error)
}

// pickVMM says which VMM a shape runs on: Firecracker, unless it asks for what
// only Cloud Hypervisor has. The shape's keys are checked here, as each provider
// owns its keys.
func pickVMM(s domain.Shape) (string, error) {
	name := vmmFirecracker
	if s.GPU > 0 {
		name = vmmCloudHypervisor
	}
	for k, v := range s.ProviderConfig {
		if k != "vmm" {
			return "", fmt.Errorf("provider_config: microvm has no key %q", k)
		}
		asked, ok := v.(string)
		if !ok || (asked != vmmFirecracker && asked != vmmCloudHypervisor) {
			return "", fmt.Errorf("provider_config: vmm must be %s or %s", vmmFirecracker, vmmCloudHypervisor)
		}
		if s.GPU > 0 && asked == vmmFirecracker {
			return "", fmt.Errorf("provider_config: Firecracker has no GPUs; shape %s needs %s", s.Name, vmmCloudHypervisor)
		}
		name = asked
	}
	return name, nil
}

// firecracker runs a machine in a jail: its own chroot, mount namespace and
// user, with Firecracker's own seccomp filters, which are on unless turned off.
type firecracker struct {
	jailer, bin string
	base        string // the jailer's --chroot-base-dir
}

func (f firecracker) jailDir(id string) string {
	return filepath.Join(f.base, filepath.Base(f.bin), id, "root")
}

// halt: Firecracker has no power management and exits on a reset.
func (firecracker) halt() string { return "reboot" }

func (firecracker) bootArgs() string { return "console=ttyS0 reboot=k panic=1 pci=off" }

func (f firecracker) command(s spec) (*exec.Cmd, error) {
	drive := func(id, file string, root, readOnly bool) map[string]any {
		return map[string]any{"drive_id": id, "path_on_host": file, "is_root_device": root, "is_read_only": readOnly}
	}
	drives := []map[string]any{drive("root", fileRoot, true, false), drive("config", fileConfig, false, false)}
	if s.Cache {
		drives = append(drives, drive("cache", fileCache, false, true))
	}
	raw, err := json.Marshal(map[string]any{
		"boot-source":        map[string]any{"kernel_image_path": fileKernel, "boot_args": s.BootArgs},
		"drives":             drives,
		"machine-config":     map[string]any{"vcpu_count": s.CPU, "mem_size_mib": s.MemoryMB},
		"network-interfaces": []map[string]any{{"iface_id": "eth0", "guest_mac": s.MAC, "host_dev_name": s.Tap}},
	})
	if err != nil {
		return nil, fmt.Errorf("encode vm config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir, fileVM), raw, 0o644); err != nil { // #nosec G306 -- the jail's user reads it; it holds no secret
		return nil, fmt.Errorf("write vm config: %w", err)
	}
	// #nosec G204 -- the operator's commands, and values of this package
	return exec.Command(f.jailer, "--id", s.ID, "--exec-file", f.bin, "--uid", strconv.Itoa(s.UID), "--gid", strconv.Itoa(s.GID),
		"--chroot-base-dir", f.base, "--", "--config-file", fileVM, "--no-api"), nil
}

// cloudHypervisor has no jailer: it runs as the machine's own user, on files only
// that user can read, with its seccomp filters on.
type cloudHypervisor struct {
	bin  string
	base string
}

func (c cloudHypervisor) jailDir(id string) string {
	return filepath.Join(c.base, vmmCloudHypervisor, id, "root")
}

// halt: Cloud Hypervisor restarts the guest on a reset and exits on a power off.
func (cloudHypervisor) halt() string { return "poweroff" }

func (cloudHypervisor) bootArgs() string { return "console=ttyS0 panic=1" }

func (c cloudHypervisor) command(s spec) (*exec.Cmd, error) {
	disk := func(file, extra string) string {
		return "path=" + filepath.Join(s.Dir, file) + ",image_type=raw" + extra
	}
	args := []string{
		"--kernel", filepath.Join(s.Dir, fileKernel), "--cmdline", s.BootArgs,
		"--cpus", "boot=" + strconv.Itoa(s.CPU), "--memory", "size=" + strconv.Itoa(s.MemoryMB) + "M",
		"--net", "tap=" + s.Tap + ",mac=" + s.MAC, "--serial", "null", "--console", "off", "--seccomp", "true",
		"--disk", disk(fileRoot, ""), disk(fileConfig, ""),
	}
	if s.Cache {
		args = append(args, disk(fileCache, ",readonly=on"))
	}
	if len(s.GPUs) > 0 {
		args = append(args, "--device")
		for _, g := range s.GPUs {
			args = append(args, "path=/sys/bus/pci/devices/"+g+"/")
		}
	}
	cmd := exec.Command(c.bin, args...) // #nosec G204 -- the operator's command, and values of this package
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
		Uid: uint32(s.UID), Gid: uint32(s.GID), // #nosec G115 -- from uid_base
		Groups: kvmGroup(),
	}}
	return cmd, nil
}

// kvmGroup is the group that opens /dev/kvm, for a VMM with no jailer to make
// it a device of its own.
func kvmGroup() []uint32 {
	var st syscall.Stat_t
	if err := syscall.Stat("/dev/kvm", &st); err != nil {
		return nil
	}
	return []uint32{st.Gid}
}

// bootArgs is the whole kernel command line of r.
func bootArgs(v vmm, n network, dns string, cache bool) string {
	parts := []string{
		v.bootArgs(), "root=" + deviceRoot, "rw",
		"forecrew.ip=" + n.Guest.String() + "/30", "forecrew.gw=" + n.Host.String(), "forecrew.dns=" + dns,
		"forecrew.root=" + deviceRoot, "forecrew.config=" + deviceConfig, "forecrew.halt=" + v.halt(),
	}
	if cache {
		parts = append(parts, "forecrew.cache="+deviceCache)
	}
	return strings.Join(append(parts, images.GuestInitArgs), " ")
}
