package core

import (
	"errors"
	"math"
	"slices"
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
)

// dispatchBattery is one battery's input to the power dispatch, powers are AC side
type dispatchBattery struct {
	mode         api.BatteryMode // mode applied to this battery
	soc          *float64
	minSoc       float64
	maxSoc       float64
	capacity     float64 // kWh, 0 = unknown
	maxCharge    float64 // W, 0 = unlimited
	maxDischarge float64 // W, 0 = unlimited
	minPower     float64 // W, the device stops below
	pv           float64 // W, own pv passing through the AC side
	running      bool    // the inverter runs with the last setpoint
}

// startPower returns the AC power the inverter needs to run
func (b dispatchBattery) startPower() float64 {
	if b.running {
		return b.minPower
	}
	return b.minPower * dispatchStartFactor
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

// canDischarge returns if mode and soc allow discharging
func (b dispatchBattery) canDischarge() bool {
	return b.mode != api.BatteryHoldDischarge && b.scaledSoc() > 0
}

// canCharge returns if mode and soc allow charging
func (b dispatchBattery) canCharge() bool {
	return b.mode != api.BatteryHoldCharge && b.scaledSoc() < 1
}

// feedIn returns the own pv, above max soc plus a discharge ramping up to max power.
// A full battery curtails its pv and reads it too low, discharging lets the pv rise to what is available.
func (b dispatchBattery) feedIn() float64 {
	return b.pv + b.aboveMaxSoc()*b.maxDischarge
}

// share returns the battery's input to the distribution
func (b dispatchBattery) share(weight float64) dispatchShare {
	lo, hi := math.Inf(-1), math.Inf(1)
	if b.maxCharge > 0 {
		lo = -b.maxCharge
	}
	if b.maxDischarge > 0 {
		hi = b.maxDischarge
	}

	// a battery that must not discharge or charge passes its own pv through
	if !b.canDischarge() {
		hi = min(hi, max(b.pv, lo))
	}
	if !b.canCharge() {
		lo = max(lo, min(b.feedIn(), hi))
	}

	return dispatchShare{
		scaledSoc: b.scaledSoc(),
		weight:    weight,
		lo:        lo,
		hi:        hi,
		preferred: max(lo, min(hi, b.feedIn())),
	}
}

// dispatchRegulating sums up the self-regulating batteries, they cannot be controlled individually
type dispatchRegulating struct {
	capacity  float64  // kWh, 0 = unknown
	scaledSoc *float64 // nil if none reports a soc
	output    float64  // W, max discharge of the batteries able to discharge, infinite if unlimited
	full      bool     // none can charge
	pv        float64  // W, pv not reported by a controlled battery
}

// sumRegulating sums up the self-regulating batteries, batteries in a forced mode are left out
func sumRegulating(bb []dispatchBattery, pv float64) dispatchRegulating {
	res := dispatchRegulating{full: true, pv: pv}

	weighted := !slices.ContainsFunc(bb, func(b dispatchBattery) bool { return b.capacity <= 0 })

	var socWeight, socAcc float64
	for _, b := range bb {
		if b.soc == nil || b.forced() {
			continue
		}

		w := 1.0
		if weighted {
			w = b.capacity
			res.capacity += b.capacity
		}
		socWeight += w
		socAcc += b.scaledSoc() * w

		if b.canDischarge() {
			if b.maxDischarge > 0 {
				res.output += b.maxDischarge
			} else {
				res.output = math.Inf(1)
			}
		}
		if b.canCharge() {
			res.full = false
		}
	}

	if socWeight > 0 {
		res.scaledSoc = new(socAcc / socWeight)
	}

	return res
}

// share returns the self-regulating batteries' input to the distribution. They charge from their own pv only.
func (r dispatchRegulating) share(weight float64) dispatchShare {
	lo := 0.0
	if r.full {
		lo = r.pv
	}

	return dispatchShare{
		scaledSoc: *r.scaledSoc,
		weight:    weight,
		lo:        lo,
		hi:        r.pv + r.output,
		preferred: r.pv,
	}
}

// dispatchShare is an inverter's input to the distribution, AC side
type dispatchShare struct {
	scaledSoc float64
	weight    float64
	lo, hi    float64 // limits, infinite if unlimited
	preferred float64 // power fed in by itself
}

// dispatchBatteryPower distributes what the batteries feed in together, demand plus their own pv, as
// AC power. Each inverter starts from its preferred power, the difference to the demand is split such that
// the socs converge. Controlled batteries below start power are stopped one by one until all running ones
// exceed it. The self-regulating batteries are not set, they take whatever is left.
// Returns the AC setpoint per controlled battery, nil for batteries that are not dispatched.
func dispatchBatteryPower(reg dispatchRegulating, bb []dispatchBattery, demand float64) []*float64 {
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
	weighted := (reg.scaledSoc == nil || reg.capacity > 0) && !slices.ContainsFunc(active, func(i int) bool { return bb[i].capacity <= 0 })
	weight := func(capacity float64) float64 {
		if weighted {
			return capacity
		}
		return 1
	}

	// controlled batteries first, the self-regulating ones last
	shares := make([]dispatchShare, 0, len(active)+1)
	total := demand
	for _, i := range active {
		shares = append(shares, bb[i].share(weight(bb[i].capacity)))
		total += bb[i].pv
	}
	if reg.scaledSoc != nil {
		shares = append(shares, reg.share(weight(reg.capacity)))
		total += reg.pv
	}

	stopped := make([]bool, len(shares))
	var ac []float64

	for {
		ac = distributeBatteryPower(shares, stopped, total)

		// stop the controlled battery furthest below its start power
		worst, margin := -1, 0.0
		for k, i := range active {
			if !stopped[k] {
				if m := math.Abs(ac[k]) - bb[i].startPower(); worst < 0 || m < margin {
					worst, margin = k, m
				}
			}
		}

		if worst < 0 || margin >= 0 {
			break
		}
		stopped[worst] = true
	}

	for k, i := range active {
		res[i] = new(ac[k])
	}

	return res
}

// distributeBatteryPower splits total between the inverters not stopped. Each starts from its preferred power,
// the difference is split by weight shifted towards the inverters further from the mean soc in the direction of
// the difference. Inverters reaching a limit stay there and the rest is split between the others.
func distributeBatteryPower(shares []dispatchShare, stopped []bool, total float64) []float64 {
	var socWeight, socAcc float64
	for _, s := range shares {
		socWeight += s.weight
		socAcc += s.scaledSoc * s.weight
	}
	meanSoc := socAcc / socWeight

	ac := make([]float64, len(shares))

	var free []int
	diff := total
	for k, s := range shares {
		if stopped[k] {
			continue
		}
		ac[k] = s.preferred
		diff -= ac[k]
		free = append(free, k)
	}

	// each round either distributes everything or pins at least one inverter to its limit
	for len(free) > 0 && diff != 0 {
		factor := make([]float64, len(shares))
		var sum float64
		for _, k := range free {
			s := shares[k]
			balance := max(-1, min(1, (s.scaledSoc-meanSoc)/dispatchBalanceSpread))
			if diff < 0 {
				balance = -balance
			}
			factor[k] = s.weight * (1 + balance)
			sum += factor[k]
		}

		// all at the far end of the balance
		if sum == 0 {
			for _, k := range free {
				factor[k] = shares[k].weight
				sum += factor[k]
			}
		}

		var rest float64
		var next []int
		for _, k := range free {
			s := shares[k]
			p := ac[k] + diff*factor[k]/sum
			ac[k] = max(s.lo, min(s.hi, p))

			if ac[k] != p {
				rest += p - ac[k]
			} else {
				next = append(next, k)
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

	var controlled, regulating []dispatchBattery
	var ctrls []api.BatteryPowerController
	var devs []config.Device[api.Meter]

	// pv not reported by a controlled battery is attributed to the self-regulating ones
	regulatingPv := max(0, state.pvPower)

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

		ctrl, ok := api.Cap[api.BatteryPowerController](meter)
		if !ok {
			regulating = append(regulating, b)
			continue
		}

		// without its pv the AC setpoint is unknown, the device keeps its last setpoint
		pv, err := ctrl.BatteryPvPower()
		if err != nil {
			site.log.ERROR.Printf("battery %s pv power: %v", deviceTitleOrName(dev), err)
			continue
		}

		b.pv = max(0, pv)
		b.minPower = ctrl.MinBatteryPower()
		b.running = site.batteryRunning[dev.Config().Name]
		regulatingPv -= b.pv

		controlled = append(controlled, b)
		ctrls = append(ctrls, ctrl)
		devs = append(devs, dev)
	}

	if len(controlled) == 0 {
		return
	}

	demand := site.dispatchDemand()
	reg := sumRegulating(regulating, max(0, regulatingPv))

	for i, p := range dispatchBatteryPower(reg, controlled, demand) {
		if p == nil {
			continue
		}

		dev := devs[i]
		if err := ctrls[i].SetBatteryPower(*p); err != nil {
			if !errors.Is(err, api.ErrNotAvailable) {
				site.log.ERROR.Printf("battery %s power: %v", deviceTitleOrName(dev), err)
			}
			continue
		}

		if site.batteryRunning == nil {
			site.batteryRunning = make(map[string]bool)
		}
		site.batteryRunning[dev.Config().Name] = *p != 0

		site.log.DEBUG.Printf("battery %s AC power setpoint: %.0fW (pv %.0fW, demand %.0fW)", deviceTitleOrName(dev), *p, controlled[i].pv, demand)
	}
}
