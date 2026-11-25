package core

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/types"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/config"
)

const (
	// dispatchWindow is the demand history the dispatched power is derived from
	dispatchWindow = 5 * time.Minute
	// dispatchPercentile of demand within the window, so short peaks neither cause export nor import
	dispatchPercentile = 0.1
	// dispatchBalanceSpread is the scaled soc difference at which balancing reaches full effect
	dispatchBalanceSpread = 0.1
	// dispatchStartFactor of min power a stopped inverter needs to start, hysteresis against toggling
	dispatchStartFactor = 1.5
	// dispatchDefaultPower is the inverter charge and discharge limit in W assumed if unknown
	dispatchDefaultPower = 800
	// dispatchDefaultCapacity is the capacity in kWh assumed if no battery's capacity is known
	dispatchDefaultCapacity = 1
)

// dispatchBattery is a battery prepared for the power dispatch, powers are AC side in W, positive = discharge
type dispatchBattery struct {
	name           string
	mode           api.BatteryMode
	scaledSoc      float64 // 0 at min soc, 1 at max soc
	overcharged    float64 // 0 up to max soc, 1 at 100%
	undercharged   float64 // 0 down to min soc, 1 at 0%
	usableCapacity float64 // kWh between min and max soc
	pv             float64 // own pv passing through the AC side
	minAC          float64
	maxAC          float64
	minPower       float64 // the inverter stops below
	running        bool    // the inverter runs with the last setpoint
}

// newDispatchBattery prepares a battery with soc for the dispatch.
// maxCharge is the AC charge limit, 0 if the battery charges from its own pv only.
func newDispatchBattery(log *util.Logger, m types.Measurement, mode api.BatteryMode, capacity, maxCharge, maxDischarge, pv float64) dispatchBattery {
	b := dispatchBattery{name: m.Title, mode: mode, pv: pv}
	if b.name == "" {
		b.name = m.Name
	}

	soc, minSoc, maxSoc := *m.Soc, 0.0, 100.0
	if m.MinSoc != nil && m.MaxSoc != nil && isFinite(*m.MinSoc) && isFinite(*m.MaxSoc) && *m.MinSoc < *m.MaxSoc {
		minSoc, maxSoc = *m.MinSoc, *m.MaxSoc
	} else {
		log.TRACE.Printf("battery %s dispatch: soc limits unknown, assuming 0..100%%", b.name)
	}

	b.scaledSoc = (soc - minSoc) / (maxSoc - minSoc)
	b.usableCapacity = capacity * (maxSoc - minSoc) / 100

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

	// below min soc charging protects the battery, above max soc discharging keeps a curtailing battery's pv reading right
	b.minAC, b.maxAC = -maxCharge, maxDischarge

	if mode == api.BatteryHoldDischarge || b.scaledSoc <= 0 {
		charge := b.undercharged * maxCharge
		if mode == api.BatteryHoldCharge {
			charge = 0
		}
		b.maxAC = min(maxDischarge, pv-charge)
	}

	if mode == api.BatteryHoldCharge || b.scaledSoc >= 1 {
		discharge := b.overcharged * maxDischarge
		if mode == api.BatteryHoldDischarge {
			discharge = 0
		}
		b.minAC = max(-maxCharge, min(b.maxAC, pv+discharge))
	}

	return b
}

// measuredCapacity returns the battery's capacity if known
func measuredCapacity(m types.Measurement) (float64, bool) {
	if m.Capacity != nil && isFinite(*m.Capacity) && *m.Capacity > 0 {
		return *m.Capacity, true
	}
	return 0, false
}

// isFinite returns if f is neither NaN nor infinite
func isFinite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}

// preferred returns the power the battery feeds in by itself: its own pv within its limits
func (b dispatchBattery) preferred() float64 {
	return max(b.minAC, min(b.maxAC, b.pv))
}

// String returns the dispatch values for logging
func (b dispatchBattery) String() string {
	return fmt.Sprintf("%s (%s): soc %.0f%% (over %.0f%%, under %.0f%%), capacity %.1fkWh, pv %.0fW, ac %.0f..%.0fW",
		b.name, b.mode, 100*b.scaledSoc, 100*b.overcharged, 100*b.undercharged, b.usableCapacity, b.pv, b.minAC, b.maxAC)
}

// dispatchBatteryPower returns the AC power per battery covering demand plus their own pv.
// Inverters below start power are stopped one by one, their share goes to the others.
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

// distributeBatteryPower splits total between the batteries not stopped, starting from their preferred power.
// The difference is weighted by usable capacity, shifted such that the socs converge.
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
	if !isFinite(power) {
		return
	}

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

// dispatchMode returns the battery's mode and if it takes part in the dispatch.
// Without soc nothing can be balanced, a forced mode owns the device.
func (site *Site) dispatchMode(dev config.Device[api.Meter], m types.Measurement) (api.BatteryMode, bool) {
	mode := site.batteryModeApplied[dev.Config().Name]
	if mode == api.BatteryUnknown {
		mode = site.GetBatteryMode()
	}

	ok := m.Soc != nil && isFinite(*m.Soc) && mode != api.BatteryCharge && mode != api.BatteryDischarge
	return mode, ok
}

// dispatchBatteries applies power setpoints to batteries that accept them
func (site *Site) dispatchBatteries() {
	state := site.state()
	devices := state.battery.Devices
	if len(devices) != len(site.batteryMeters) {
		return
	}

	// first pass: pv not measured by a controlled battery belongs to the self-regulating ones
	ctrls := make([]api.BatteryPowerController, len(devices))
	pvs := make([]float64, len(devices))

	var regulatingPv float64
	if isFinite(state.pvPower) {
		regulatingPv = max(0, state.pvPower)
	}

	var knownCapacity, regulatingCapacity float64
	var known, regulatingUnknown int
	var controlled bool

	for i, dev := range site.batteryMeters {
		if _, ok := site.dispatchMode(dev, devices[i]); !ok {
			continue
		}

		c, ok := measuredCapacity(devices[i])
		if ok {
			knownCapacity += c
			known++
		}

		ctrl, ok := api.Cap[api.BatteryPowerController](dev.Instance())
		if !ok {
			if c > 0 {
				regulatingCapacity += c
			} else {
				regulatingUnknown++
			}
			continue
		}

		ctrls[i] = ctrl
		controlled = true

		if p, err := ctrl.BatteryPvPower(); err != nil {
			site.log.ERROR.Printf("battery %s pv power: %v", deviceTitleOrName(dev), err)
		} else if !isFinite(p) {
			site.log.ERROR.Printf("battery %s pv power: invalid value %v", deviceTitleOrName(dev), p)
		} else {
			pvs[i] = max(0, p)
			regulatingPv -= pvs[i]
		}
	}

	if !controlled {
		return
	}

	defaultCapacity := float64(dispatchDefaultCapacity)
	if known > 0 {
		defaultCapacity = knownCapacity / float64(known)
	}
	regulatingCapacity += float64(regulatingUnknown) * defaultCapacity
	regulatingPv = max(0, regulatingPv)

	// second pass: prepare all batteries alike
	var bb []dispatchBattery
	var idx []int

	for i, dev := range site.batteryMeters {
		m := devices[i]
		mode, ok := site.dispatchMode(dev, m)
		if !ok {
			continue
		}

		capacity, ok := measuredCapacity(m)
		if !ok {
			capacity = defaultCapacity
			site.log.TRACE.Printf("battery %s dispatch: capacity unknown, assuming %.1fkWh", deviceTitleOrName(dev), capacity)
		}

		maxCharge, maxDischarge := float64(dispatchDefaultPower), float64(dispatchDefaultPower)
		var limitsKnown bool
		if bpl, ok := api.Cap[api.BatteryPowerLimiter](dev.Instance()); ok {
			if c, d, err := bpl.GetPowerLimits(); err == nil && isFinite(c) && isFinite(d) && c > 0 && d > 0 {
				maxCharge, maxDischarge, limitsKnown = c, d, true
			}
		}
		if !limitsKnown {
			site.log.TRACE.Printf("battery %s dispatch: power limits unknown, assuming %dW", deviceTitleOrName(dev), dispatchDefaultPower)
		}

		pv := pvs[i]
		if ctrls[i] == nil {
			// self-regulating batteries charge from their own pv only, which passes the inverter on top of the discharge
			pv = regulatingPv * capacity / regulatingCapacity
			maxCharge, maxDischarge = 0, maxDischarge+pv
		}

		b := newDispatchBattery(site.log, m, mode, capacity, maxCharge, maxDischarge, pv)
		if ctrls[i] != nil {
			b.minPower = ctrls[i].MinBatteryPower()
			b.running = site.batteryRunning[dev.Config().Name]
		}

		site.log.TRACE.Printf("battery dispatch: %v", b)

		bb = append(bb, b)
		idx = append(idx, i)
	}

	demand := site.dispatchDemand()

	// the self-regulating batteries are not set, they take whatever is left
	for k, p := range dispatchBatteryPower(bb, demand) {
		i := idx[k]
		if ctrls[i] == nil {
			continue
		}

		if err := ctrls[i].SetBatteryPower(p); err != nil {
			if !errors.Is(err, api.ErrNotAvailable) {
				site.log.ERROR.Printf("battery %s power: %v", bb[k].name, err)
			}
			continue
		}

		if site.batteryRunning == nil {
			site.batteryRunning = make(map[string]bool)
		}
		site.batteryRunning[site.batteryMeters[i].Config().Name] = p != 0

		site.log.DEBUG.Printf("battery %s AC power setpoint: %.0fW (demand %.0fW)", bb[k].name, p, demand)
	}
}
