package worker

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestProtectedProcess is the helper process of TestProcessProtection.
func TestProtectedProcess(t *testing.T) {
	if os.Getenv("WORKER_HELPER_PROTECT") != "1" {
		t.Skip("helper process")
	}
	if err := ProtectProcess(); err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("dumpable:", dumpable())
	// An agent is a child of the same user: it must not read the environment.
	cmd := exec.Command("cat", fmt.Sprintf("/proc/%d/environ", os.Getpid()))
	out, err := cmd.CombinedOutput()
	fmt.Println("environ readable:", err == nil && strings.Contains(string(out), "WORKER_HELPER_PROTECT"))
	// A child that leaves an orphan: the orphan becomes our child.
	if err := exec.Command("sh", "-c", "sleep 30 &").Run(); err != nil {
		fmt.Println("error:", err)
		return
	}
	adopted := 0
	for _, p := range scanProcs("") {
		if p.ppid == os.Getpid() {
			adopted++
		}
	}
	fmt.Println("adopted orphans:", adopted)
	killLeftovers("", true, nil)
}

func TestProcessProtection(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestProtectedProcess$", "-test.v")
	cmd.Env = append(hermeticEnv(), "WORKER_HELPER_PROTECT=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper: %v\n%s", err, out)
	}
	for _, want := range []string{"dumpable: 0", "environ readable: false", "adopted orphans: 1"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("helper output lacks %q:\n%s", want, out)
		}
	}
}
