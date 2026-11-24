package core

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util/config"
)

const (
	// dispatchWindow is the demand history the dispatched power is derived from
	dispatchWindow = 5 * time.Minute
	// dispatchPercentile of demand within the window, so short peaks neither cause export nor import
	dispatchPercentile = 0.1
	// dispatchBalanceSpread is the scaled soc difference at which balancing reaches full effect
	dispatchBalanceSpread = 0.1
	// dispatchStartFactor of min power a stopped inverter needs to start, a running one keeps running down to min power
	dispatchStartFactor = 1.5
	// dispatchDefaultPower is the inverter charge and discharge limit in W assumed if unknown
	dispatchDefaultPower = 800
	// dispatchDefaultCapacity is the capacity in kWh assumed if no battery's capacity is known
	dispatchDefaultCapacity = 1
)

// dispatchBattery is a battery prepared for the power dispatch, powers are AC side in W, positive = discharge
type dispatchBattery struct {
	// input
	name         string
	controlled   bool // accepts a power setpoint, otherwise self-regulating
	mode         api.BatteryMode
	soc          float64
	minSoc       float64
	maxSoc       float64
	capacity     float64 // kWh, 0 = unknown
	maxCharge    float64 // 0 = unknown
	maxDischarge float64 // 0 = unknown
	pv           float64 // own pv passing through the AC side, unknown for self-regulating batteries
	minPower     float64 // the inverter stops below
	running      bool    // the inverter runs with the last setpoint

	// prepared
	capacityAssumed bool
	limitsAssumed   bool
	scaledSoc       float64 // 0 at min soc, 1 at max soc
	overcharged     float64 // 0 up to max soc, 1 at 100%
	undercharged    float64 // 0 down to min soc, 1 at 0%
	usableCapacity  float64 // kWh between min and max soc
	full            bool    // must not charge
	minAC           float64
	maxAC           float64
	preferred       float64 // power fed in by itself
}

// assumeCapacities sets unknown capacities to the average of the known ones, or the default if none is known
func assumeCapacities(bb []dispatchBattery) {
	var sum float64
	var known int
	for _, b := range bb {
		if b.capacity > 0 {
			sum += b.capacity
			known++
		}
	}

	assumed := float64(dispatchDefaultCapacity)
	if known > 0 {
		assumed = sum / float64(known)
	}

	for i := range bb {
		if bb[i].capacity <= 0 {
			bb[i].capacity = assumed
			bb[i].capacityAssumed = true
		}
	}
}

// prepare derives the dispatch values from the input
func (b *dispatchBattery) prepare() {
	if b.maxSoc <= b.minSoc {
		b.minSoc, b.maxSoc = 0, 100
	}

	b.scaledSoc = (b.soc - b.minSoc) / (b.maxSoc - b.minSoc)
	b.usableCapacity = b.capacity * (b.maxSoc - b.minSoc) / 100

	switch {
	case b.soc >= 100:
		b.overcharged = 1
	case b.soc > b.maxSoc:
		b.overcharged = (b.soc - b.maxSoc) / (100 - b.maxSoc)
	}
	switch {
	case b.soc <= 0:
		b.undercharged = 1
	case b.soc < b.minSoc:
		b.undercharged = (b.minSoc - b.soc) / b.minSoc
	}

	if b.maxCharge <= 0 || b.maxDischarge <= 0 {
		b.limitsAssumed = true
		if b.maxCharge <= 0 {
			b.maxCharge = dispatchDefaultPower
		}
		if b.maxDischarge <= 0 {
			b.maxDischarge = dispatchDefaultPower
		}
	}

	b.full = b.mode == api.BatteryHoldCharge || b.scaledSoc >= 1
	empty := b.mode == api.BatteryHoldDischarge || b.scaledSoc <= 0

	if !b.controlled {
		// self-regulating batteries charge from their own pv only, which is added for all of them together
		if !empty {
			b.maxAC = b.maxDischarge
		}
		return
	}

	// a battery that must not discharge or charge passes its own pv through.
	// Above max soc it discharges in addition: a full battery curtails its pv and reads it too low.
	b.maxAC = b.maxDischarge
	if empty {
		b.maxAC = min(b.pv, b.maxDischarge)
	}

	b.preferred = min(b.maxAC, b.pv+b.overcharged*b.maxDischarge)

	b.minAC = -b.maxCharge
	if b.full {
		b.minAC = b.preferred
	}
}

// sumRegulating sums up the prepared self-regulating batteries, they cannot be controlled individually.
// pv is their pv, which is passed through if none of them can charge.
func sumRegulating(bb []dispatchBattery, pv float64) dispatchBattery {
	res := dispatchBattery{name: "self-regulating", pv: pv, full: true}

	for _, b := range bb {
		res.usableCapacity += b.usableCapacity
		res.scaledSoc += b.scaledSoc * b.usableCapacity
		res.overcharged += b.overcharged * b.usableCapacity
		res.undercharged += b.undercharged * b.usableCapacity
		res.maxAC += b.maxAC
		res.full = res.full && b.full
		res.capacityAssumed = res.capacityAssumed || b.capacityAssumed
		res.limitsAssumed = res.limitsAssumed || b.limitsAssumed
	}

	res.scaledSoc /= res.usableCapacity
	res.overcharged /= res.usableCapacity
	res.undercharged /= res.usableCapacity

	res.maxAC += pv
	res.preferred = pv
	if res.full {
		res.minAC = pv
	}

	return res
}

// dispatchTable formats the prepared batteries, assumed values are marked with *
func dispatchTable(bb []dispatchBattery) string {
	assumed := func(format string, v float64, a bool) string {
		if a {
			format += "*"
		}
		return fmt.Sprintf(format, v)
	}

	var sb strings.Builder
	w := tabwriter.NewWriter(&sb, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "battery\tmode\tsoc %\tscaled\tover\tunder\tcapacity kWh\tpv W\tmin W\tmax W\tpreferred W\t")

	for _, b := range bb {
		mode, soc, pv := "", "", fmt.Sprintf("%.0f", b.pv)
		if b.name != "self-regulating" {
			mode, soc = b.mode.String(), fmt.Sprintf("%.0f", b.soc)
			if !b.controlled {
				pv = "*"
			}
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%.2f\t%.2f\t%.2f\t%s\t%s\t%s\t%s\t%.0f\t\n",
			b.name, mode, soc, b.scaledSoc, b.overcharged, b.undercharged,
			assumed("%.1f", b.usableCapacity, b.capacityAssumed), pv,
			assumed("%.0f", b.minAC, b.limitsAssumed), assumed("%.0f", b.maxAC, b.limitsAssumed), b.preferred)
	}

	w.Flush()
	return sb.String()
}

// dispatchBatteryPower distributes what the batteries feed in together, demand plus their own pv, as
// AC power. Each battery starts from its preferred power, the difference to the demand is split by usable
// capacity, shifted such that the socs converge. Batteries below start power are stopped one by one until
// all running ones exceed it.
// Returns the AC power per battery.
func dispatchBatteryPower(bb []dispatchBattery, demand float64) []float64 {
	total := demand
	for _, b := range bb {
		total += b.pv
	}

	stopped := make([]bool, len(bb))

	for {
		ac := distributeBatteryPower(bb, stopped, total)

		// stop the battery furthest below its start power
		worst, margin := -1, 0.0
		for i, b := range bb {
			if stopped[i] {
				continue
			}

			start := b.minPower
			if !b.running {
				start *= dispatchStartFactor
			}

			if m := math.Abs(ac[i]) - start; worst < 0 || m < margin {
				worst, margin = i, m
			}
		}

		if worst < 0 || margin >= 0 {
			return ac
		}
		stopped[worst] = true
	}
}

// distributeBatteryPower splits total between the batteries not stopped. Each starts from its preferred power,
// the difference is split by usable capacity shifted towards the batteries further from the mean soc in the
// direction of the difference. Batteries reaching a limit stay there and the rest is split between the others.
func distributeBatteryPower(bb []dispatchBattery, stopped []bool, total float64) []float64 {
	var capacity, socAcc float64
	for _, b := range bb {
		capacity += b.usableCapacity
		socAcc += b.scaledSoc * b.usableCapacity
	}
	meanSoc := socAcc / capacity

	ac := make([]float64, len(bb))

	var free []int
	diff := total
	for i, b := range bb {
		if stopped[i] {
			continue
		}
		ac[i] = b.preferred
		diff -= ac[i]
		free = append(free, i)
	}

	// each round either distributes everything or pins at least one battery to its limit
	for len(free) > 0 && diff != 0 {
		factor := make([]float64, len(bb))
		var sum float64
		for _, i := range free {
			b := bb[i]
			balance := max(-1, min(1, (b.scaledSoc-meanSoc)/dispatchBalanceSpread))
			if diff < 0 {
				balance = -balance
			}
			factor[i] = b.usableCapacity * (1 + balance)
			sum += factor[i]
		}

		// all at the far end of the balance
		if sum == 0 {
			for _, i := range free {
				factor[i] = bb[i].usableCapacity
				sum += factor[i]
			}
		}

		var rest float64
		var next []int
		for _, i := range free {
			p := ac[i] + diff*factor[i]/sum
			ac[i] = max(bb[i].minAC, min(bb[i].maxAC, p))

			if ac[i] != p {
				rest += p - ac[i]
			} else {
				next = append(next, i)
			}
		}

		diff, free = rest, next
	}

	return ac
}

type demandSample struct {
	ts    time.Time
	power float64
}

// recordDemand adds a sample of the demand the batteries cover together to the dispatch window
func (site *Site) recordDemand(now time.Time, power float64) {
	site.demandWindow = append(site.demandWindow, demandSample{now, power})
	site.demandWindow = slices.DeleteFunc(site.demandWindow, func(s demandSample) bool {
		return now.Sub(s.ts) > dispatchWindow
	})
}

// dispatchDemand returns the demand the batteries can cover without exporting or importing within the
// window: the low percentile if it's a deficit, the high percentile if it's a surplus, else zero.
func (site *Site) dispatchDemand() float64 {
	if len(site.demandWindow) == 0 {
		return 0
	}

	vals := make([]float64, 0, len(site.demandWindow))
	for _, s := range site.demandWindow {
		vals = append(vals, s.power)
	}
	slices.Sort(vals)

	n := float64(len(vals) - 1)
	low := vals[int(math.Floor(dispatchPercentile*n))]
	high := vals[int(math.Ceil((1-dispatchPercentile)*n))]

	switch {
	case low > 0:
		return low
	case high < 0:
		return high
	default:
		return 0
	}
}

// dispatchBatteries applies power setpoints to batteries that accept them
func (site *Site) dispatchBatteries() {
	state := site.state()
	devices := state.battery.Devices
	if len(devices) != len(site.batteryMeters) {
		return
	}

	var bb []dispatchBattery
	ctrls := make(map[int]api.BatteryPowerController)
	devs := make(map[int]config.Device[api.Meter])

	for i, dev := range site.batteryMeters {
		meter := dev.Instance()
		m := devices[i]

		mode := site.batteryModeApplied[dev.Config().Name]
		if mode == api.BatteryUnknown {
			mode = site.GetBatteryMode()
		}

		// without soc nothing can be balanced, a forced mode owns the device
		if m.Soc == nil || mode == api.BatteryCharge || mode == api.BatteryDischarge {
			continue
		}

		b := dispatchBattery{name: deviceTitleOrName(dev), mode: mode, soc: *m.Soc, maxSoc: 100}
		if m.MinSoc != nil {
			b.minSoc = *m.MinSoc
		}
		if m.MaxSoc != nil {
			b.maxSoc = *m.MaxSoc
		}
		if m.Capacity != nil {
			b.capacity = *m.Capacity
		}

		if bpl, ok := api.Cap[api.BatteryPowerLimiter](meter); ok {
			if c, d, err := bpl.GetPowerLimits(); err == nil {
				b.maxCharge, b.maxDischarge = c, d
			}
		}

		if ctrl, ok := api.Cap[api.BatteryPowerController](meter); ok {
			// without its pv the AC setpoint is unknown, the device keeps its last setpoint
			pv, err := ctrl.BatteryPvPower()
			if err != nil {
				site.log.ERROR.Printf("battery %s pv power: %v", b.name, err)
				continue
			}

			b.controlled = true
			b.pv = max(0, pv)
			b.minPower = ctrl.MinBatteryPower()
			b.running = site.batteryRunning[dev.Config().Name]

			ctrls[len(bb)] = ctrl
			devs[len(bb)] = dev
		}

		bb = append(bb, b)
	}

	if len(ctrls) == 0 {
		return
	}

	assumeCapacities(bb)

	// controlled batteries first, the self-regulating ones summed up last
	var dispatched, regulating []dispatchBattery
	var idx []int

	// pv not reported by a controlled battery belongs to the self-regulating ones
	regulatingPv := max(0, state.pvPower)

	for i := range bb {
		bb[i].prepare()
		if bb[i].controlled {
			dispatched = append(dispatched, bb[i])
			idx = append(idx, i)
			regulatingPv -= bb[i].pv
		} else {
			regulating = append(regulating, bb[i])
		}
	}

	rows := bb
	if len(regulating) > 0 {
		sum := sumRegulating(regulating, max(0, regulatingPv))
		dispatched = append(dispatched, sum)
		rows = append(slices.Clone(bb), sum)
	}

	site.log.TRACE.Printf("battery dispatch:\n%s", dispatchTable(rows))

	demand := site.dispatchDemand()
	ac := dispatchBatteryPower(dispatched, demand)

	// setpoints for the controlled batteries, which come first
	for k, i := range idx {
		b, p := bb[i], ac[k]

		if err := ctrls[i].SetBatteryPower(p); err != nil {
			if !errors.Is(err, api.ErrNotAvailable) {
				site.log.ERROR.Printf("battery %s power: %v", b.name, err)
			}
			continue
		}

		if site.batteryRunning == nil {
			site.batteryRunning = make(map[string]bool)
		}
		site.batteryRunning[devs[i].Config().Name] = p != 0

		site.log.DEBUG.Printf("battery %s AC power setpoint: %.0fW (demand %.0fW)", b.name, p, demand)
	}
}
