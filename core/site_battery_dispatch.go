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
	"github.com/evcc-io/evcc/core/types"
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
	name            string
	mode            api.BatteryMode
	scaledSoc       float64 // 0 at min soc, 1 at max soc
	overcharged     float64 // 0 up to max soc, 1 at 100%
	undercharged    float64 // 0 down to min soc, 1 at 0%
	usableCapacity  float64 // kWh between min and max soc
	capacityAssumed bool
	pv              float64 // own pv passing through the AC side
	minAC           float64
	maxAC           float64
	limitsAssumed   bool
	minPower        float64 // the inverter stops below
	running         bool    // the inverter runs with the last setpoint
}

// newDispatchBattery prepares a battery with soc for the dispatch. defaultCapacity is used if the capacity
// is unknown. maxCharge is the AC charge limit, 0 if the battery charges from its own pv only.
func newDispatchBattery(m types.Measurement, mode api.BatteryMode, defaultCapacity, maxCharge, maxDischarge, pv float64) dispatchBattery {
	b := dispatchBattery{name: m.Title, mode: mode, pv: pv}
	if b.name == "" {
		b.name = m.Name
	}

	soc, minSoc, maxSoc := *m.Soc, 0.0, 100.0
	if m.MinSoc != nil && m.MaxSoc != nil && *m.MinSoc < *m.MaxSoc {
		minSoc, maxSoc = *m.MinSoc, *m.MaxSoc
	}

	b.scaledSoc = (soc - minSoc) / (maxSoc - minSoc)

	switch {
	case soc >= 100:
		b.overcharged = 1
	case soc > maxSoc:
		b.overcharged = (soc - maxSoc) / (100 - maxSoc)
	}
	switch {
	case soc <= 0:
		b.undercharged = 1
	case soc < minSoc:
		b.undercharged = (minSoc - soc) / minSoc
	}

	capacity, known := measuredCapacity(m, defaultCapacity)
	b.capacityAssumed = !known
	b.usableCapacity = capacity * (maxSoc - minSoc) / 100

	// a battery that must not discharge or charge passes its own pv through.
	// Above max soc it discharges in addition: a full battery curtails its pv and reads it too low.
	b.maxAC = maxDischarge
	if mode == api.BatteryHoldDischarge || b.scaledSoc <= 0 {
		b.maxAC = min(pv, maxDischarge)
	}

	b.minAC = -maxCharge
	if mode == api.BatteryHoldCharge || b.scaledSoc >= 1 {
		b.minAC = min(b.maxAC, pv+b.overcharged*maxDischarge)
	}

	return b
}

// measuredCapacity returns the battery's capacity, or defaultCapacity if unknown
func measuredCapacity(m types.Measurement, defaultCapacity float64) (float64, bool) {
	if m.Capacity != nil && *m.Capacity > 0 {
		return *m.Capacity, true
	}
	return defaultCapacity, false
}

// preferred returns the power the battery feeds in by itself: its own pv within its limits
func (b dispatchBattery) preferred() float64 {
	return max(b.minAC, min(b.maxAC, b.pv))
}

// averageCapacity returns the average of the known capacities, or the default if none is known
func averageCapacity(mm []types.Measurement) float64 {
	var sum float64
	var known int
	for _, m := range mm {
		if m.Capacity != nil && *m.Capacity > 0 {
			sum += *m.Capacity
			known++
		}
	}

	if known == 0 {
		return dispatchDefaultCapacity
	}
	return sum / float64(known)
}

// sumRegulating sums up the self-regulating batteries, they cannot be controlled individually
func sumRegulating(bb []dispatchBattery) dispatchBattery {
	res := dispatchBattery{name: "self-regulating"}

	for _, b := range bb {
		res.pv += b.pv
		res.minAC += b.minAC
		res.usableCapacity += b.usableCapacity
		res.scaledSoc += b.scaledSoc * b.usableCapacity
		res.overcharged += b.overcharged * b.usableCapacity
		res.undercharged += b.undercharged * b.usableCapacity
		res.maxAC += b.maxAC
		res.capacityAssumed = res.capacityAssumed || b.capacityAssumed
		res.limitsAssumed = res.limitsAssumed || b.limitsAssumed
	}

	res.scaledSoc /= res.usableCapacity
	res.overcharged /= res.usableCapacity
	res.undercharged /= res.usableCapacity

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
	fmt.Fprintln(w, "battery\tmode\tscaled soc\tover\tunder\tcapacity kWh\tpv W\tmin W\tmax W\t")

	for _, b := range bb {
		mode := ""
		if b.mode != api.BatteryUnknown {
			mode = b.mode.String()
		}

		fmt.Fprintf(w, "%s\t%s\t%.2f\t%.2f\t%.2f\t%s\t%.0f\t%s\t%s\t\n",
			b.name, mode, b.scaledSoc, b.overcharged, b.undercharged,
			assumed("%.1f", b.usableCapacity, b.capacityAssumed), b.pv,
			assumed("%.0f", b.minAC, b.limitsAssumed), assumed("%.0f", b.maxAC, b.limitsAssumed))
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
		ac[i] = b.preferred()
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

	defaultCapacity := averageCapacity(devices)

	// controlled batteries first, the self-regulating ones summed up last
	var dispatched []dispatchBattery
	var ctrls []api.BatteryPowerController
	var devs []config.Device[api.Meter]

	// pv not reported by a controlled battery belongs to the self-regulating ones
	regulatingPv := max(0, state.pvPower)

	type regulatingInput struct {
		m             types.Measurement
		mode          api.BatteryMode
		maxDischarge  float64
		limitsAssumed bool
	}
	var pending []regulatingInput

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

		maxCharge, maxDischarge := float64(dispatchDefaultPower), float64(dispatchDefaultPower)
		var limitsKnown bool
		if bpl, ok := api.Cap[api.BatteryPowerLimiter](meter); ok {
			if c, d, err := bpl.GetPowerLimits(); err == nil && c > 0 && d > 0 {
				maxCharge, maxDischarge, limitsKnown = c, d, true
			}
		}

		ctrl, controlled := api.Cap[api.BatteryPowerController](meter)
		if !controlled {
			// prepared once their pv is known
			pending = append(pending, regulatingInput{m, mode, maxDischarge, !limitsKnown})
			continue
		}

		var pv float64
		if p, err := ctrl.BatteryPvPower(); err == nil {
			pv = max(0, p)
			regulatingPv -= pv
		} else {
			site.log.ERROR.Printf("battery %s pv power: %v", deviceTitleOrName(dev), err)
		}

		b := newDispatchBattery(m, mode, defaultCapacity, maxCharge, maxDischarge, pv)
		b.limitsAssumed = !limitsKnown
		b.minPower = ctrl.MinBatteryPower()
		b.running = site.batteryRunning[dev.Config().Name]

		dispatched = append(dispatched, b)
		ctrls = append(ctrls, ctrl)
		devs = append(devs, dev)
	}

	if len(ctrls) == 0 {
		return
	}

	// the self-regulating batteries' pv is only known in sum, it is split by capacity
	var regulatingCapacity float64
	for _, r := range pending {
		c, _ := measuredCapacity(r.m, defaultCapacity)
		regulatingCapacity += c
	}

	var regulating []dispatchBattery
	for _, r := range pending {
		c, _ := measuredCapacity(r.m, defaultCapacity)
		pv := max(0, regulatingPv) * c / regulatingCapacity

		// they charge from their own pv only, which passes the inverter in addition to the battery's discharge
		b := newDispatchBattery(r.m, r.mode, defaultCapacity, 0, r.maxDischarge+pv, pv)
		b.limitsAssumed = r.limitsAssumed
		regulating = append(regulating, b)
	}

	rows := append(slices.Clone(dispatched), regulating...)
	if len(regulating) > 0 {
		sum := sumRegulating(regulating)
		dispatched = append(dispatched, sum)
		rows = append(rows, sum)
	}

	site.log.TRACE.Printf("battery dispatch:\n%s", dispatchTable(rows))

	demand := site.dispatchDemand()
	ac := dispatchBatteryPower(dispatched, demand)

	// the controlled batteries come first
	for i, ctrl := range ctrls {
		b, p := dispatched[i], ac[i]

		if err := ctrl.SetBatteryPower(p); err != nil {
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
