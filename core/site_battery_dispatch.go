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
	// dispatchWindow is the home power history the discharging headroom is derived from
	dispatchWindow = 5 * time.Minute
	// dispatchPercentile of home power within the window, so short load peaks don't cause export
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
	batteryPower float64         // W, measured at the battery, positive = discharge
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

// allows returns if the mode and soc allow the battery to discharge or charge
func (b dispatchBattery) allows(discharging bool) bool {
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

// dispatchBatteryPower determines the share of the net battery power the controlled batteries take
// together and fills it into as few of them as possible. The self-regulating batteries take the
// remainder. headroom caps the discharge, since the self-regulating inverter cannot absorb export.
// Returns the setpoint per battery, nil for batteries that are not dispatched.
func dispatchBatteryPower(bb []dispatchBattery, headroom float64) []*float64 {
	res := make([]*float64, len(bb))

	// like the site soc, weigh by capacity only if all capacities are known
	weighted := !slices.ContainsFunc(bb, func(b dispatchBattery) bool { return b.capacity <= 0 })
	weight := func(b dispatchBattery) float64 {
		if weighted {
			return b.capacity
		}
		return 1
	}

	var netPower, totalWeight, scaledSocAcc float64
	for _, b := range bb {
		netPower += b.batteryPower
		if b.soc != nil {
			totalWeight += weight(b)
			scaledSocAcc += b.scaledSoc() * weight(b)
		}
	}
	if totalWeight == 0 {
		return res
	}
	mean := scaledSocAcc / totalWeight

	// all batteries follow the direction of the net power, opposite directions would move energy between them
	discharging := netPower >= 0

	// the controlled batteries' share: each battery's fair share shifted by its distance from the mean
	var total float64
	var candidates []int
	for i, b := range bb {
		if !b.controlled || b.soc == nil {
			continue
		}

		// forced modes own the device
		if b.mode == api.BatteryCharge || b.mode == api.BatteryDischarge {
			continue
		}

		res[i] = new(0.0)
		if !b.allows(discharging) {
			continue
		}

		balance := max(-1, min(1, (b.scaledSoc()-mean)/dispatchBalanceSpread))
		if !discharging {
			balance = -balance
		}

		total += math.Abs(netPower) * weight(b) / totalWeight * (1 + balance)
		candidates = append(candidates, i)
	}

	if discharging {
		total = min(total, max(0, headroom))
	}

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

// recordHomePower adds a home power sample to the dispatch window
func (site *Site) recordHomePower(now time.Time, power float64) {
	site.homePowerWindow = append(site.homePowerWindow, timedPower{now, power})
	site.homePowerWindow = slices.DeleteFunc(site.homePowerWindow, func(s timedPower) bool {
		return now.Sub(s.ts) > dispatchWindow
	})
}

// homePowerPercentile returns the given percentile of the home power window
func (site *Site) homePowerPercentile(p float64) float64 {
	if len(site.homePowerWindow) == 0 {
		return 0
	}
	vals := make([]float64, 0, len(site.homePowerWindow))
	for _, s := range site.homePowerWindow {
		vals = append(vals, s.power)
	}
	slices.Sort(vals)
	return vals[int(math.Floor(p*float64(len(vals)-1)))]
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

		b := dispatchBattery{batteryPower: m.Power, soc: m.Soc, maxSoc: 100}
		if m.MinSoc != nil {
			b.minSoc = *m.MinSoc
		}
		if m.MaxSoc != nil && *m.MaxSoc > 0 {
			b.maxSoc = *m.MaxSoc
		}
		if m.Capacity != nil {
			b.capacity = *m.Capacity
		}

		if ctrl, ok := api.Cap[api.BatteryPowerController](meter); ok {
			ctrls[i] = ctrl
			b.controlled = true
			controlled = true

			b.mode = site.batteryModeApplied[dev.Config().Name]
			if b.mode == api.BatteryUnknown {
				b.mode = site.GetBatteryMode()
			}

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

	headroom := site.homePowerPercentile(dispatchPercentile) - site.BatteryDispatchReserve

	for i, p := range dispatchBatteryPower(bb, headroom) {
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

		site.log.DEBUG.Printf("battery %s power setpoint: %.0fW (headroom %.0fW)", deviceTitleOrName(dev), *p, headroom)
	}
}
