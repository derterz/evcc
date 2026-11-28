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
	// dispatchWindow is the consumption history the dispatched power is derived from
	dispatchWindow = 10 * time.Minute
	// dispatchPercentile of consumption within the window, low and high, so short peaks neither cause export nor import
	dispatchPercentile = 0.1
	// dispatchBalanceSpread is the scaled soc difference at which balancing reaches full effect
	dispatchBalanceSpread = 0.1
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
	usableCapacity float64 // kWh between min and max soc
	pv             float64 // own pv passing through the AC side
	targetCharge   float64 // charge below min soc, negative = discharge above max soc, exchanged with the grid instead of the other batteries
	minAC          float64
	maxAC          float64
	minPower       float64 // the inverter stops below
}

// newDispatchBattery prepares a battery with soc for the dispatch.
// maxCharge is the AC charge limit, 0 if the battery charges from its own pv only.
func newDispatchBattery(log *util.Logger, m types.Measurement, mode api.BatteryMode, capacity, maxCharge, maxDischarge, pv float64) dispatchBattery {
	b := dispatchBattery{name: measurementName(m), mode: mode, pv: pv}

	soc, minSoc, maxSoc := *m.Soc, 0.0, 100.0
	if m.MinSoc != nil && m.MaxSoc != nil && isFinite(*m.MinSoc) && isFinite(*m.MaxSoc) && *m.MinSoc < *m.MaxSoc {
		minSoc, maxSoc = *m.MinSoc, *m.MaxSoc
	} else {
		log.TRACE.Printf("battery %s dispatch: soc limits unknown, assuming 0..100%%", b.name)
	}

	b.scaledSoc = (soc - minSoc) / (maxSoc - minSoc)
	b.usableCapacity = capacity * (maxSoc - minSoc) / 100

	// below min soc charging protects the battery, above max soc discharging keeps a curtailing battery's pv reading right
	var charge, discharge float64
	switch {
	case soc <= 0:
		charge = maxCharge
	case soc < minSoc:
		charge = maxCharge * (minSoc - soc) / minSoc
	}
	switch {
	case soc >= 100:
		discharge = maxDischarge
	case soc > maxSoc:
		discharge = maxDischarge * (soc - maxSoc) / (100 - maxSoc)
	}
	b.targetCharge = charge - discharge
	target := pv - b.targetCharge

	b.minAC, b.maxAC = -maxCharge, maxDischarge
	if mode == api.BatteryHoldDischarge || b.scaledSoc <= 0 {
		b.maxAC = min(maxDischarge, target)
	}
	if mode == api.BatteryHoldCharge || b.scaledSoc >= 1 {
		b.minAC = max(-maxCharge, min(b.maxAC, target))
	}

	// only what is reachable within the limits, beyond the pv the battery passes anyway
	b.targetCharge = max(-maxCharge, min(maxDischarge, pv)) - b.preferred()

	return b
}

// pinnedDispatchBattery prepares a battery fixed at ac, its soc only takes part in the balancing if known
func pinnedDispatchBattery(log *util.Logger, m types.Measurement, mode api.BatteryMode, capacity, pv, ac float64) dispatchBattery {
	b := dispatchBattery{name: measurementName(m), mode: mode, pv: pv}
	if hasSoc(m) {
		b = newDispatchBattery(log, m, mode, capacity, 0, 0, pv)
	}
	b.minAC, b.maxAC, b.targetCharge = ac, ac, 0
	return b
}

// measurementName returns the battery's title or name
func measurementName(m types.Measurement) string {
	if m.Title != "" {
		return m.Title
	}
	return m.Name
}

// hasSoc returns if the battery's soc is known
func hasSoc(m types.Measurement) bool {
	return m.Soc != nil && isFinite(*m.Soc)
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

// preferred returns the power the battery feeds in by itself: its own pv minus the target charge within its limits
func (b dispatchBattery) preferred() float64 {
	return max(b.minAC, min(b.maxAC, b.pv-b.targetCharge))
}

// String returns the dispatch values for logging
func (b dispatchBattery) String() string {
	return fmt.Sprintf("%s (%s): soc %.0f%%, capacity %.1fkWh, pv %.0fW, target charge %.0fW, ac %.0f..%.0fW",
		b.name, b.mode, 100*b.scaledSoc, b.usableCapacity, b.pv, b.targetCharge, b.minAC, b.maxAC)
}

// dispatchBatteryPower returns the AC power per battery covering total.
// Inverters below min power are stopped one by one, their share goes to the others.
func dispatchBatteryPower(bb []dispatchBattery, total float64) []float64 {
	stopped := make([]bool, len(bb))

	for {
		ac := distributeBatteryPower(bb, stopped, total)

		// stop the battery furthest below its min power
		worst, margin := -1, 0.0
		for i, b := range bb {
			if stopped[i] {
				continue
			}

			if m := math.Abs(ac[i]) - b.minPower; worst < 0 || m < margin {
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
		if b.minAC < b.maxAC {
			free = append(free, i)
		}
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

type consumptionSample struct {
	ts    time.Time
	power float64
}

// recordConsumption adds a consumption sample to the dispatch window and drops samples older than the window
func (site *Site) recordConsumption(now time.Time, power float64) {
	if !isFinite(power) || power < 0 {
		return
	}

	site.consumptionWindow = append(site.consumptionWindow, consumptionSample{now, power})
	site.pruneConsumption(now)
}

// pruneConsumption drops samples older than the window
func (site *Site) pruneConsumption(now time.Time) {
	site.consumptionWindow = slices.DeleteFunc(site.consumptionWindow, func(s consumptionSample) bool {
		return now.Sub(s.ts) > dispatchWindow
	})
}

// consumptionPercentiles returns the low and high percentile of consumption within the window
func (site *Site) consumptionPercentiles() (float64, float64) {
	if len(site.consumptionWindow) == 0 {
		return 0, 0
	}

	vals := make([]float64, 0, len(site.consumptionWindow))
	for _, s := range site.consumptionWindow {
		vals = append(vals, s.power)
	}
	slices.Sort(vals)

	n := float64(len(vals) - 1)
	return vals[int(math.Floor(dispatchPercentile*n))], vals[int(math.Ceil((1-dispatchPercentile)*n))]
}

// dispatchTotal returns the AC power the batteries cover together for the low and high consumption.
// Their target charge is exchanged with the grid. Beyond their preferred power they only discharge
// if even the low consumption is a deficit and only charge if even the high consumption is a surplus.
func dispatchTotal(bb []dispatchBattery, low, high float64) float64 {
	var preferred, targetCharge float64
	for _, b := range bb {
		preferred += b.preferred()
		targetCharge += b.targetCharge
	}

	switch {
	case low-targetCharge > preferred:
		return low - targetCharge
	case high-targetCharge < preferred:
		return high - targetCharge
	default:
		return preferred
	}
}

// dispatchMode returns the battery's mode and if it takes part in the dispatch.
func (site *Site) dispatchMode(dev config.Device[api.Meter]) (api.BatteryMode, bool) {
	mode := site.batteryModeApplied[dev.Config().Name]
	if mode == api.BatteryUnknown {
		mode = site.GetBatteryMode()
	}

	ok := mode != api.BatteryCharge && mode != api.BatteryDischarge
	return mode, ok
}

// dispatchBatteries applies power setpoints to batteries that accept them and stops those that left the dispatch
func (site *Site) dispatchBatteries() {
	site.releaseBatteries(site.applyBatteryDispatch(time.Now()))
}

// releaseBatteries stops batteries dispatched before but not anymore, unless a forced mode owns them now
func (site *Site) releaseBatteries(dispatched map[string]bool) {
	if dispatched == nil {
		dispatched = make(map[string]bool)
	}

	for _, dev := range site.batteryMeters {
		name := dev.Config().Name
		if !site.batteryDispatched[name] || dispatched[name] {
			continue
		}

		if _, ok := site.dispatchMode(dev); !ok {
			continue
		}

		ctrl, ok := api.Cap[api.BatteryPowerController](dev.Instance())
		if !ok {
			continue
		}

		if err := ctrl.SetBatteryPower(0); err != nil && !errors.Is(err, api.ErrNotAvailable) {
			site.log.ERROR.Printf("battery %s power: %v", deviceTitleOrName(dev), err)
			// retry next cycle
			dispatched[name] = true
			continue
		}

		site.log.DEBUG.Printf("battery %s left the dispatch, stopped", deviceTitleOrName(dev))
	}

	site.batteryDispatched = dispatched
}

// applyBatteryDispatch applies power setpoints and returns the batteries it set
func (site *Site) applyBatteryDispatch(now time.Time) map[string]bool {
	state := site.state()
	devices := state.battery.Devices
	if len(devices) != len(site.batteryMeters) {
		return nil
	}

	// without recent consumption, e.g. while meters fail, there is nothing to dispatch from
	site.pruneConsumption(now)
	if len(site.consumptionWindow) == 0 {
		return nil
	}

	// first pass: pv not measured by a battery's own controller belongs to the uncontrolled ones
	ctrls := make([]api.BatteryPowerController, len(devices))
	pvs := make([]float64, len(devices))
	measured := make([]bool, len(devices))

	var additionalPv float64
	if isFinite(state.pvPower) {
		additionalPv = max(0, state.pvPower)
	}

	var knownCapacity, uncontrolledCapacity float64
	var known, uncontrolledUnknown int
	var foundControlled bool

	for i, dev := range site.batteryMeters {
		c, ok := measuredCapacity(devices[i])
		if ok {
			knownCapacity += c
			known++
		}

		ctrl, ok := api.Cap[api.BatteryPowerController](dev.Instance())
		if !ok {
			if c > 0 {
				uncontrolledCapacity += c
			} else {
				uncontrolledUnknown++
			}
			continue
		}

		measured[i] = true
		if p, err := ctrl.BatteryPvPower(); err != nil {
			site.log.ERROR.Printf("battery %s pv power: %v", deviceTitleOrName(dev), err)
		} else if !isFinite(p) {
			site.log.ERROR.Printf("battery %s pv power: invalid value %v", deviceTitleOrName(dev), p)
		} else {
			pvs[i] = max(0, p)
			additionalPv -= pvs[i]
		}

		if _, ok := site.dispatchMode(dev); ok && hasSoc(devices[i]) {
			ctrls[i] = ctrl
			foundControlled = true
		}
	}

	if !foundControlled {
		return nil
	}

	low, high := site.consumptionPercentiles()

	defaultCapacity := float64(dispatchDefaultCapacity)
	if known > 0 {
		defaultCapacity = knownCapacity / float64(known)
	}
	uncontrolledCapacity += float64(uncontrolledUnknown) * defaultCapacity
	additionalPv = max(0, additionalPv)

	var estimatedPvPerCapacity float64
	if uncontrolledCapacity == 0 {
		// no battery can pass this pv, so it comes from pv inverters without battery
		low -= additionalPv
		high -= additionalPv
	} else {
		estimatedPvPerCapacity = additionalPv / uncontrolledCapacity
	}

	// second pass: prepare all batteries alike
	var bb []dispatchBattery
	var idx []int

	for i, dev := range site.batteryMeters {
		m := devices[i]
		mode, modeOk := site.dispatchMode(dev)

		capacity, ok := measuredCapacity(m)
		if !ok {
			capacity = defaultCapacity
			site.log.TRACE.Printf("battery %s dispatch: capacity unknown, assuming %.1fkWh", deviceTitleOrName(dev), capacity)
		}

		pv := pvs[i]
		if !measured[i] {
			pv = estimatedPvPerCapacity * capacity
		}

		var b dispatchBattery
		switch {
		case !modeOk:
			// a forced mode owns the device, the others take its current power as given
			ac := pv
			if isFinite(m.Power) {
				ac += m.Power
			}
			b = pinnedDispatchBattery(site.log, m, mode, capacity, pv, ac)

		case !hasSoc(m):
			// nothing to balance, assumed to pass its pv through
			b = pinnedDispatchBattery(site.log, m, mode, capacity, pv, pv)

		default:
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

			if ctrls[i] == nil {
				// uncontrolled batteries charge from their own pv only, the discharge limit caps the inverter's AC output including that pv
				maxCharge = 0
			}

			b = newDispatchBattery(site.log, m, mode, capacity, maxCharge, maxDischarge, pv)
			if ctrls[i] != nil {
				b.minPower = ctrls[i].MinBatteryPower()
			}
		}

		site.log.TRACE.Printf("battery dispatch: %v", b)

		bb = append(bb, b)
		idx = append(idx, i)
	}

	// the uncontrolled batteries are not set, they take whatever is left
	total := dispatchTotal(bb, low, high)
	site.log.DEBUG.Printf("battery dispatch: consumption %.0f..%.0fW, total %.0fW", low, high, total)

	dispatched := make(map[string]bool)

	for k, p := range dispatchBatteryPower(bb, total) {
		i := idx[k]
		if ctrls[i] == nil {
			continue
		}

		// also after an error, so the battery is stopped once it leaves the dispatch
		dispatched[site.batteryMeters[i].Config().Name] = true

		if err := ctrls[i].SetBatteryPower(p); err != nil {
			if !errors.Is(err, api.ErrNotAvailable) {
				site.log.ERROR.Printf("battery %s power: %v", bb[k].name, err)
			}
			continue
		}

		site.log.DEBUG.Printf("battery %s AC power setpoint: %.0fW", bb[k].name, p)
	}

	return dispatched
}
