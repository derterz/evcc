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
	// dispatchFullSpread is the scaled soc range below full in which a battery increasingly passes its own pv through
	dispatchFullSpread = 0.1
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
	pv           float64 // W, own pv passing through the AC side
	setpoint     float64 // W, last battery side setpoint applied
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

// fullness ramps from 0 to 1 within dispatchFullSpread below full, 1 if the battery cannot charge
func (b dispatchBattery) fullness() float64 {
	if !b.allows(false) {
		return 1
	}
	return max(0, min(1, (b.scaledSoc()-1+dispatchFullSpread)/dispatchFullSpread))
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
// Returns the AC setpoint per battery, nil for batteries that are not dispatched.
func dispatchBatteryPower(bb []dispatchBattery, demand, consumption float64) []*float64 {
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
		b := bb[i]

		p := total
		if limit := b.maxPower(discharging); limit > 0 {
			// the limits apply on the AC side, where the own pv adds to discharging and offsets charging
			if discharging {
				limit -= b.pv
			} else {
				limit += b.pv
			}
			p = min(p, limit)
		}
		if p <= 0 {
			continue
		}

		signed := p
		if !discharging {
			signed = -p
		}

		// too small to run this inverter, a later one or the self-regulating batteries take it
		if math.Abs(signed+b.pv) < b.minPower {
			continue
		}

		total -= p
		*res[i] = signed
	}

	// AC side
	var ac, allowed float64
	var dispatched []int
	for i, b := range bb {
		if res[i] != nil {
			dispatched = append(dispatched, i)
			*res[i] += b.pv
			ac += *res[i]
			// near full the own pv is passed through even if exported, so it isn't curtailed
			allowed += b.fullness() * max(0, b.pv)
		}
	}

	// the self-regulating batteries only charge from their own pv, AC beyond the consumption would be exported
	excess := ac - consumption - allowed

	slices.SortStableFunc(dispatched, func(i, j int) int {
		return cmp.Compare(bb[j].priority(false), bb[i].priority(false))
	})

	for _, i := range dispatched {
		if excess <= 0 {
			break
		}

		b := bb[i]
		if !b.allows(false) {
			continue
		}

		lower := math.Inf(-1)
		if b.maxCharge > 0 {
			lower = -b.maxCharge
		}

		red := min(excess, *res[i]-lower)
		if red <= 0 {
			continue
		}

		*res[i] -= red
		excess -= red

		if math.Abs(*res[i]) < b.minPower {
			*res[i] = 0
		}
	}

	return res
}

type demandSample struct {
	ts          time.Time
	demand      float64
	consumption float64
}

// recordDemand adds a sample of the demand the batteries cover and the consumption to the dispatch window
func (site *Site) recordDemand(now time.Time, demand, consumption float64) {
	site.demandWindow = append(site.demandWindow, demandSample{now, demand, consumption})
	site.demandWindow = slices.DeleteFunc(site.demandWindow, func(s demandSample) bool {
		return now.Sub(s.ts) > dispatchWindow
	})
}

// percentiles returns the low and high percentile of the values
func percentiles(vals []float64) (float64, float64) {
	slices.Sort(vals)
	n := float64(len(vals) - 1)
	return vals[int(math.Floor(dispatchPercentile*n))], vals[int(math.Ceil((1-dispatchPercentile)*n))]
}

// dispatchDemand returns the demand the batteries can cover without exporting or importing within the
// window: the low percentile if it's a deficit, the high percentile if it's a surplus, else zero.
// Also returns the low percentile of consumption, the AC power that can be fed in without exporting.
func (site *Site) dispatchDemand() (float64, float64) {
	if len(site.demandWindow) == 0 {
		return 0, 0
	}

	demand := make([]float64, 0, len(site.demandWindow))
	consumption := make([]float64, 0, len(site.demandWindow))
	for _, s := range site.demandWindow {
		demand = append(demand, s.demand)
		consumption = append(consumption, s.consumption)
	}

	low, high := percentiles(demand)
	minConsumption, _ := percentiles(consumption)
	minConsumption = max(0, minConsumption)

	switch {
	case low > 0:
		return low, minConsumption
	case high < 0:
		return high, minConsumption
	default:
		return 0, minConsumption
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

			// without its pv the AC setpoint is unknown, the device keeps its last setpoint
			if pv, err := ctrl.BatteryPvPower(); err == nil {
				b.pv = max(0, pv)
			} else {
				site.log.ERROR.Printf("battery %s pv power: %v", deviceTitleOrName(dev), err)
				ctrls[i] = nil
			}
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

	demand, consumption := site.dispatchDemand()

	for i, p := range dispatchBatteryPower(bb, demand, consumption) {
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

		if site.batterySetpoints == nil {
			site.batterySetpoints = make(map[string]float64)
		}
		site.batterySetpoints[dev.Config().Name] = *p - bb[i].pv

		site.log.DEBUG.Printf("battery %s AC power setpoint: %.0fW (pv %.0fW, demand %.0fW, consumption %.0fW)", deviceTitleOrName(dev), *p, bb[i].pv, demand, consumption)
	}
}
