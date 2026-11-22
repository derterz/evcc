package core

import (
	"cmp"
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
	// dispatchSwitchSpread is the scaled soc lead another battery needs to take over from a running one
	dispatchSwitchSpread = 0.05
)

// dispatchBattery is one battery's input to the power dispatch
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
	setpoint     float64 // W, last setpoint applied
}

// scaledSoc returns the soc rescaled to the range between min and max soc, 0..1
func (b dispatchBattery) scaledSoc() float64 {
	lo, hi := b.minSoc, b.maxSoc
	if hi <= lo {
		lo, hi = 0, 100
	}
	return (*b.soc - lo) / (hi - lo)
}

// forced returns if a forced mode owns the device
func (b dispatchBattery) forced() bool {
	return b.mode == api.BatteryCharge || b.mode == api.BatteryDischarge
}

// allows returns if the mode and soc allow the battery to discharge or charge
func (b dispatchBattery) allows(discharging bool) bool {
	if b.soc == nil || b.forced() {
		return false
	}
	if discharging {
		return b.mode != api.BatteryHoldDischarge && b.scaledSoc() > 0
	}
	return b.mode != api.BatteryHoldCharge && b.scaledSoc() < 1
}

// maxPower returns the power limit in the given direction, 0 = unlimited
func (b dispatchBattery) maxPower(discharging bool) float64 {
	if discharging {
		return b.maxDischarge
	}
	return b.maxCharge
}

// priority orders the batteries to fill: fullest first when discharging, emptiest first when charging.
// A running battery keeps its place until another leads by dispatchSwitchSpread.
func (b dispatchBattery) priority(discharging bool) float64 {
	prio := b.scaledSoc()
	if !discharging {
		prio = -prio
	}
	if discharging && b.setpoint > 0 || !discharging && b.setpoint < 0 {
		prio += dispatchSwitchSpread
	}
	return prio
}

// dispatchBatteryPower splits the demand the batteries cover together, positive = discharge, between
// the batteries able to take it and fills the controlled batteries' part into as few of them as possible.
// The self-regulating batteries take the remainder.
// Returns the setpoint per battery, nil for batteries that are not dispatched.
func dispatchBatteryPower(bb []dispatchBattery, demand float64) []*float64 {
	res := make([]*float64, len(bb))

	// like the site soc, weigh by capacity only if all capacities are known
	weighted := !slices.ContainsFunc(bb, func(b dispatchBattery) bool { return b.capacity <= 0 })
	weight := func(b dispatchBattery) float64 {
		if weighted {
			return b.capacity
		}
		return 1
	}

	// all batteries follow the direction of the demand, opposite directions would move energy between them
	discharging := demand > 0

	var socWeight, scaledSocAcc, takeWeight float64
	for _, b := range bb {
		if b.soc == nil {
			continue
		}
		socWeight += weight(b)
		scaledSocAcc += b.scaledSoc() * weight(b)

		// a full or empty battery's share goes to the others
		if b.allows(discharging) {
			takeWeight += weight(b)
		}
	}

	// the controlled batteries' part: each battery's share shifted by its distance from the mean
	var total float64
	var candidates []int
	for i, b := range bb {
		if !b.controlled || b.soc == nil || b.forced() {
			continue
		}

		res[i] = new(0.0)
		if !b.allows(discharging) {
			continue
		}

		balance := max(-1, min(1, (b.scaledSoc()-scaledSocAcc/socWeight)/dispatchBalanceSpread))
		if !discharging {
			balance = -balance
		}

		total += math.Abs(demand) * weight(b) / takeWeight * (1 + balance)
		candidates = append(candidates, i)
	}

	// more than the demand would make the self-regulating batteries compensate in the opposite direction
	total = min(total, math.Abs(demand))

	slices.SortStableFunc(candidates, func(i, j int) int {
		return cmp.Compare(bb[j].priority(discharging), bb[i].priority(discharging))
	})

	for _, i := range candidates {
		p := total
		if limit := bb[i].maxPower(discharging); limit > 0 {
			p = min(p, limit)
		}

		// too small to run this inverter, a later one or the self-regulating batteries take it
		if p < bb[i].minPower || p <= 0 {
			continue
		}

		total -= p
		if !discharging {
			p = -p
		}
		*res[i] = p
	}

	return res
}

type timedPower struct {
	ts    time.Time
	power float64
}

// recordDemand adds a demand sample to the dispatch window
func (site *Site) recordDemand(now time.Time, power float64) {
	site.demandWindow = append(site.demandWindow, timedPower{now, power})
	site.demandWindow = slices.DeleteFunc(site.demandWindow, func(s timedPower) bool {
		return now.Sub(s.ts) > dispatchWindow
	})
}

// dispatchDemand returns the demand the batteries can cover without exporting or importing within the
// window: the low percentile if it's a deficit, the high percentile if it's a surplus, else zero.
// reserve is left to the self-regulating batteries in either direction.
func (site *Site) dispatchDemand(reserve float64) float64 {
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
	case low-reserve > 0:
		return low - reserve
	case high+reserve < 0:
		return high + reserve
	default:
		return 0
	}
}

// dispatchBatteries applies power setpoints to batteries that accept them
func (site *Site) dispatchBatteries() {
	devices := site.state().battery.Devices
	if len(devices) != len(site.batteryMeters) {
		return
	}

	bb := make([]dispatchBattery, len(site.batteryMeters))
	ctrls := make([]api.BatteryPowerController, len(site.batteryMeters))

	var controlled bool
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

		if ctrl, ok := api.Cap[api.BatteryPowerController](meter); ok {
			ctrls[i] = ctrl
			b.controlled = true
			controlled = true

			b.minPower = ctrl.MinBatteryPower()
			b.setpoint = site.batterySetpoints[dev.Config().Name]

			if bpl, ok := api.Cap[api.BatteryPowerLimiter](meter); ok {
				if c, d, err := bpl.GetPowerLimits(); err == nil {
					b.maxCharge, b.maxDischarge = c, d
				}
			}
		}

		bb[i] = b
	}

	if !controlled {
		return
	}

	demand := site.dispatchDemand(site.BatteryDispatchReserve)

	for i, p := range dispatchBatteryPower(bb, demand) {
		if p == nil {
			continue
		}

		dev := site.batteryMeters[i]
		if err := ctrls[i].SetBatteryPower(*p); err != nil {
			if !errors.Is(err, api.ErrNotAvailable) {
				site.log.ERROR.Printf("battery %s power: %v", deviceTitleOrName(dev), err)
			}
			continue
		}

		if site.batterySetpoints == nil {
			site.batterySetpoints = make(map[string]float64)
		}
		site.batterySetpoints[dev.Config().Name] = *p

		site.log.DEBUG.Printf("battery %s power setpoint: %.0fW (demand %.0fW)", deviceTitleOrName(dev), *p, demand)
	}
}
