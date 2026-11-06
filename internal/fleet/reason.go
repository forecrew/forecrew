package fleet

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
)

// ReasonCode says why a queued job waits for a machine, one per row of the
// waiting table of the configuration and fleet UI.
type ReasonCode string

// Reason codes.
const (
	ReasonBooting ReasonCode = "booting"          // a machine is starting for it
	ReasonQueue   ReasonCode = "queue"            // it waits its turn on a machine type or worker
	ReasonOffline ReasonCode = "location_offline" // only locations that cannot start machines can run it
	ReasonNoShape ReasonCode = "no_machine_type"  // nothing can run it
	ReasonLimit   ReasonCode = "machine_limit"    // a limit of machines at once is reached
	ReasonBudget  ReasonCode = "budget"           // a monthly limit is used up
	ReasonPolicy  ReasonCode = "policy"           // something could run it, but the policy says no
	ReasonZone    ReasonCode = "trust_zone"       // nothing of its pool is online
)

// Reason is why a job waits: a code to act on and the sentence people read.
// The zero Reason means it does not wait for anything: a worker is about to
// lease it.
type Reason struct {
	Code     ReasonCode `json:"code"`
	Sentence string     `json:"sentence"`
}

// displayName turns a technical name into what people read: home-server is
// Home Server.
func displayName(name string) string {
	words := strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' || r == '.' })
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

// zoneName is what people call a pool (UI wording table).
func zoneName(pool string) string {
	switch pool {
	case domain.PoolControl:
		return "Reasoning"
	case domain.PoolActions:
		return "GitHub Actions"
	case "device":
		return "Devices"
	case "prod":
		return "Production"
	}
	return displayName(pool)
}

// ordinal spells a place in line.
func ordinal(n int) string {
	names := []string{"First", "Second", "Third", "Fourth", "Fifth", "Sixth", "Seventh", "Eighth", "Ninth", "Tenth"}
	if n >= 1 && n <= len(names) {
		return names[n-1]
	}
	suffix := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return strconv.Itoa(n) + suffix
}

// plural counts things: 1 machine, 5 machines.
func plural(n int, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	return strconv.Itoa(n) + " " + thing + "s"
}

// money formats an amount in the instance's currency.
func money(currency string, amount float64) string {
	return currency + " " + strconv.FormatFloat(amount, 'f', -1, 64)
}

// joinAnd joins a list as a sentence does.
func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// maxLabelsShown bounds a sentence: labels can come from a repo.
const maxLabelsShown = 5

// plainLabel keeps text a repo could put in a label out of the sentences.
var plainLabel = regexp.MustCompile(`^[a-z0-9][a-z0-9:._+-]{0,63}$`)

// describe says what a job asks of a machine: Windows with 16 GB.
func describe(d domain.ShapeDemand) string {
	what, details := "a machine", []string{}
	if r := d.Resources; r.MinMemoryMB > 0 {
		if r.MinMemoryMB >= 1024 {
			details = append(details, strconv.Itoa((r.MinMemoryMB+1023)/1024)+" GB")
		} else {
			details = append(details, strconv.Itoa(r.MinMemoryMB)+" MB")
		}
	}
	if n := d.Resources.MinCPU; n > 0 {
		details = append(details, plural(n, "CPU"))
	}
	if n := d.Resources.GPU; n > 0 {
		details = append(details, plural(n, "GPU"))
	}
	if n := d.Resources.MinDiskGB; n > 0 {
		details = append(details, strconv.Itoa(n)+" GB of disk")
	}
	shown := 0
	for _, l := range d.Labels {
		switch {
		case l == "windows":
			what = "Windows"
		case l == "macos" || l == "darwin":
			what = "macOS"
		case l == "linux":
			what = "Linux"
		case plainLabel.MatchString(l) && shown < maxLabelsShown:
			details = append(details, l)
			shown++
		}
	}
	if len(details) == 0 {
		return what
	}
	return what + " with " + joinAnd(details)
}

func bootingReason(estimate time.Duration) Reason {
	r := Reason{Code: ReasonBooting, Sentence: "Starting a machine"}
	if estimate > 0 {
		r.Sentence += fmt.Sprintf(", about %d seconds", int(estimate.Round(5*time.Second).Seconds()))
	}
	return r
}

func queueReason(place int, what string) Reason {
	return Reason{Code: ReasonQueue, Sentence: ordinal(place) + " in line for " + what}
}

func offlineReason(names []string, status string) Reason {
	slices.Sort(names)
	names = slices.Compact(names)
	if len(names) == 1 {
		return Reason{Code: ReasonOffline, Sentence: "Only " + names[0] + " can run this, and it is " + status}
	}
	return Reason{Code: ReasonOffline, Sentence: joinAnd(names) + " can run this, and none of them is online"}
}

func noShapeReason(d domain.ShapeDemand) Reason {
	return Reason{Code: ReasonNoShape, Sentence: "Nothing here runs " + describe(d)}
}

func zoneReason(pool string) Reason {
	return Reason{Code: ReasonZone, Sentence: "No machine in the " + zoneName(pool) + " zone is online"}
}

func policyReason(location string, routing domain.FleetRouting) Reason {
	rest := "your policy does not allow it"
	if routing == domain.RoutingOwnOnly {
		rest = "your policy keeps jobs on your machines"
	}
	return Reason{Code: ReasonPolicy, Sentence: location + " could run this, but " + rest}
}
