package fleet

import (
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
)

func TestSentences(t *testing.T) {
	for got, want := range map[string]string{
		displayName("mac-mini_2.local"):  "Mac Mini 2 Local",
		zoneName(domain.PoolBuild):       "Build",
		zoneName(domain.PoolControl):     "Reasoning",
		zoneName(domain.PoolActions):     "GitHub Actions",
		zoneName("device"):               "Devices",
		zoneName("prod"):                 "Production",
		ordinal(3):                       "Third",
		ordinal(11):                      "11th",
		ordinal(12):                      "12th",
		ordinal(21):                      "21st",
		ordinal(22):                      "22nd",
		ordinal(23):                      "23rd",
		ordinal(24):                      "24th",
		plural(1, "job"):                 "1 job",
		plural(2, "job"):                 "2 jobs",
		money("CHF", 200):                "CHF 200",
		money("USD", 0.04):               "USD 0.04",
		joinAnd(nil):                     "",
		joinAnd([]string{"a"}):           "a",
		joinAnd([]string{"a", "b", "c"}): "a, b and c",
	} {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestDescribe(t *testing.T) {
	tests := []struct {
		d    domain.ShapeDemand
		want string
	}{
		{domain.ShapeDemand{}, "a machine"},
		{domain.ShapeDemand{Labels: []string{"windows"}, Resources: domain.Resources{MinMemoryMB: 16384}}, "Windows with 16 GB"},
		{domain.ShapeDemand{Labels: []string{"linux"}, Resources: domain.Resources{MinMemoryMB: 512, MinCPU: 1}}, "Linux with 512 MB and 1 CPU"},
		{domain.ShapeDemand{Labels: []string{"darwin", "macos", "xcode:16"}}, "macOS with xcode:16"},
		{domain.ShapeDemand{Resources: domain.Resources{MinCPU: 4, GPU: 2, MinDiskGB: 100}}, "a machine with 4 CPUs, 2 GPUs and 100 GB of disk"},
	}
	for _, tt := range tests {
		if got := describe(tt.d); got != tt.want {
			t.Errorf("describe(%+v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}
