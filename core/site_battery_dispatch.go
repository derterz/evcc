package core

import (
	"errors"
	"math"
	"slices"
	"time"

	"github.com/evcc-io/evcc/api"
)

const (
	// dispatchWindow is the demand history the dispatched power is derived from
	dispatchWindow = 5 * time.Minute
	// dispatchPercentile of demand within the window, so short peaks neither cause export nor import
	dispatchPercentile = 0.1
	// dispatchBalanceSpread is the scaled soc difference at which balancing reaches full effect
	dispatchBalanceSpread = 0.1
)

// dispatchBattery is one battery's input to the power dispatch, powers are AC side
type dispatchBattery struct {
	controlled   bool            // accepts a power setpoint, otherwise self-regulating
	mode         api.BatteryMode // mode applied to this battery
	soc          *float64
	minSoc       float64
	maxSoc       float64
	capacity     float64 // kWh, 0 = unknown
	maxCharge    float64 // W, 0 = unlimited
	maxDischarge float64 // W, 0 = unlimited
	minPower     float64 // W, the device stops below
	pv           float64 // W, own pv passing through the AC side
}

// scaledSoc returns the soc rescaled to the range between min and max soc, 0..1
func (b dispatchBattery) scaledSoc() float64 {
	lo, hi := b.minSoc, b.maxSoc
	if hi <= lo {
		lo, hi = 0, 100
	}
	return (*b.soc - lo) / (hi - lo)
}

// aboveMaxSoc ramps from 0 at max soc to 1 at 100%
func (b dispatchBattery) aboveMaxSoc() float64 {
	switch {
	case *b.soc >= 100:
		return 1
	case b.maxSoc >= 100:
		return 0
	default:
		return max(0, min(1, (*b.soc-b.maxSoc)/(100-b.maxSoc)))
	}
}

// forced returns if a forced mode owns the device
func (b dispatchBattery) forced() bool {
	return b.mode == api.BatteryCharge || b.mode == api.BatteryDischarge
}

// limits returns the AC power range the device limits, mode and soc allow
func (b dispatchBattery) limits() (float64, float64) {
	lo, hi := math.Inf(-1), math.Inf(1)
	if b.maxCharge > 0 {
		lo = -b.maxCharge
	}
	if !b.controlled {
		// self-regulating batteries charge from their own pv only
		lo = 0
	}
	if b.maxDischarge > 0 {
		hi = b.maxDischarge
	}

	// a battery that must not discharge or charge passes its own pv through
	if b.mode == api.BatteryHoldDischarge || b.scaledSoc() <= 0 {
		hi = min(hi, max(b.pv, lo))
	}
	if b.mode == api.BatteryHoldCharge || b.scaledSoc() >= 1 {
		floor := b.pv
		if b.controlled {
			// a full battery curtails its pv and reads it too low, discharging lets the pv rise to what is available
			floor += b.aboveMaxSoc() * b.maxDischarge
		}
		lo = max(lo, min(floor, hi))
	}

	return lo, hi
}

// preferred returns the AC power the battery feeds in by itself, its own pv within its limits
func (b dispatchBattery) preferred() float64 {
	lo, hi := b.limits()
	return max(lo, min(hi, b.pv))
}

// dispatchBatteryPower distributes what the batteries feed in together, demand plus their own pv, as
// AC power. Each battery starts from its preferred power, the difference to the demand is split such that
// the socs converge. Controlled batteries below min power are stopped one by one until all running ones
// exceed it. The self-regulating batteries' part is not set, they take whatever is left.
// Returns the AC setpoint per controlled battery, nil for batteries that are not dispatched.
func dispatchBatteryPower(bb []dispatchBattery, demand float64) []*float64 {
	res := make([]*float64, len(bb))

	var active []int
	for i, b := range bb {
		if b.soc != nil && !b.forced() {
			active = append(active, i)
		}
	}
	if len(active) == 0 {
		return res
	}

	// like the site soc, weigh by capacity only if all capacities are known
	weighted := !slices.ContainsFunc(active, func(i int) bool { return bb[i].capacity <= 0 })
	weight := func(b dispatchBattery) float64 {
		if weighted {
			return b.capacity
		}
		return 1
	}

	total := demand
	var socWeight, socAcc float64
	for _, i := range active {
		b := bb[i]
		total += b.pv
		socWeight += weight(b)
		socAcc += b.scaledSoc() * weight(b)
	}
	meanSoc := socAcc / socWeight

	stopped := make([]bool, len(bb))
	var ac []float64

	for {
		ac = distributeBatteryPower(bb, active, stopped, total, weight, meanSoc)

		// stop the controlled battery furthest below its min power
		worst, margin := -1, 0.0
		for _, i := range active {
			if b := bb[i]; b.controlled && !stopped[i] {
				if m := math.Abs(ac[i]) - b.minPower; worst < 0 || m < margin {
					worst, margin = i, m
				}
			}
		}

		if worst < 0 || margin >= 0 {
			break
		}
		stopped[worst] = true
	}

	for _, i := range active {
		if bb[i].controlled {
			res[i] = new(ac[i])
		}
	}

	return res
}

// distributeBatteryPower splits total between the batteries not stopped. Each starts from its preferred power,
// the difference is split by weight shifted towards the batteries further from the mean soc in the direction of
// the difference. Batteries reaching a limit stay there and the rest is split between the others.
func distributeBatteryPower(bb []dispatchBattery, active []int, stopped []bool, total float64, weight func(dispatchBattery) float64, meanSoc float64) []float64 {
	ac := make([]float64, len(bb))

	var free []int
	diff := total
	for _, i := range active {
		if stopped[i] {
			continue
		}
		ac[i] = bb[i].preferred()
		diff -= ac[i]
		free = append(free, i)
	}

	// each round either distributes everything or pins at least one battery to its limit
	for len(free) > 0 && diff != 0 {
		factor := make([]float64, len(bb))
		var sum float64
		for _, i := range free {
			b := bb[i]
			balance := max(-1, min(1, (b.scaledSoc()-meanSoc)/dispatchBalanceSpread))
			if diff < 0 {
				balance = -balance
			}
			factor[i] = weight(b) * (1 + balance)
			sum += factor[i]
		}

		// all at the far end of the balance
		if sum == 0 {
			for _, i := range free {
				factor[i] = weight(bb[i])
				sum += factor[i]
			}
		}

		var rest float64
		var next []int
		for _, i := range free {
			lo, hi := bb[i].limits()
			p := ac[i] + diff*factor[i]/sum
			ac[i] = max(lo, min(hi, p))

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

	bb := make([]dispatchBattery, len(site.batteryMeters))
	ctrls := make([]api.BatteryPowerController, len(site.batteryMeters))

	// pv not reported by a controlled battery is attributed to the self-regulating ones
	regulatingPv := max(0, state.pvPower)
	var regulating int

	for i, dev := range site.batteryMeters {
		meter := dev.Instance()
		m := devices[i]

		b := dispatchBattery{soc: m.Soc, maxSoc: 100}
		if m.MinSoc != nil {
			b.minSoc = *m.MinSoc
		}
		if m.MaxSoc != nil && *m.MaxSoc > 0 {
			b.maxSoc = *m.MaxSoc
		}
		if m.Capacity != nil {
			b.capacity = *m.Capacity
		}

		b.mode = site.batteryModeApplied[dev.Config().Name]
		if b.mode == api.BatteryUnknown {
			b.mode = site.GetBatteryMode()
		}

		if bpl, ok := api.Cap[api.BatteryPowerLimiter](meter); ok {
			if c, d, err := bpl.GetPowerLimits(); err == nil {
				b.maxCharge, b.maxDischarge = c, d
			}
		}

		if ctrl, ok := api.Cap[api.BatteryPowerController](meter); ok {
			b.controlled = true
			b.minPower = ctrl.MinBatteryPower()

			// without its pv the AC setpoint is unknown, the device keeps its last setpoint
			if pv, err := ctrl.BatteryPvPower(); err == nil {
				ctrls[i] = ctrl
				b.pv = max(0, pv)
				regulatingPv -= b.pv
			} else {
				site.log.ERROR.Printf("battery %s pv power: %v", deviceTitleOrName(dev), err)
			}
		} else {
			regulating++
		}

		bb[i] = b
	}

	if !slices.ContainsFunc(ctrls, func(c api.BatteryPowerController) bool { return c != nil }) {
		return
	}

	for i := range bb {
		if !bb[i].controlled && regulating > 0 {
			bb[i].pv = max(0, regulatingPv) / float64(regulating)
		}
	}

	demand := site.dispatchDemand()

	for i, p := range dispatchBatteryPower(bb, demand) {
		if p == nil || ctrls[i] == nil {
			continue
		}

		dev := site.batteryMeters[i]
		if err := ctrls[i].SetBatteryPower(*p); err != nil {
			if !errors.Is(err, api.ErrNotAvailable) {
				site.log.ERROR.Printf("battery %s power: %v", deviceTitleOrName(dev), err)
			}
			continue
		}

		site.log.DEBUG.Printf("battery %s AC power setpoint: %.0fW (pv %.0fW, demand %.0fW)", deviceTitleOrName(dev), *p, bb[i].pv, demand)
	}
}
