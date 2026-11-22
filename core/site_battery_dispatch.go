package core

import (
	"errors"
	"math"
	"slices"
	"time"

	"github.com/evcc-io/evcc/api"
)

const (
	// dispatchWindow is the home power history the discharge headroom is derived from
	dispatchWindow = 5 * time.Minute
	// dispatchPercentile of home power within the window, so short load peaks don't cause export
	dispatchPercentile = 0.1
	// dispatchBalanceSpread is the usable soc difference at which balancing reaches full effect
	dispatchBalanceSpread = 0.1
)

// dispatchBattery is one battery's input to the power dispatch
type dispatchBattery struct {
	controlled   bool            // accepts a power setpoint, otherwise self-regulating
	mode         api.BatteryMode // mode applied to this battery
	power        float64         // measured power, positive = discharge
	soc          *float64
	minSoc       float64
	maxSoc       float64
	capacity     float64 // kWh, 0 = unknown
	maxCharge    float64 // W, 0 = unlimited
	maxDischarge float64 // W, 0 = unlimited
	minPower     float64 // W, the device stops below
}

// usable returns the soc as fraction of the usable range between min and max soc
func (b dispatchBattery) usable() float64 {
	lo, hi := b.minSoc, b.maxSoc
	if hi <= lo {
		lo, hi = 0, 100
	}
	return (*b.soc - lo) / (hi - lo)
}

// dispatchBatteryPower distributes the current net battery power across the controlled batteries.
// The self-regulating batteries take the remainder. headroom caps the total discharge of the controlled
// batteries, since the self-regulating inverter cannot absorb their export.
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

	var net, totalWeight, usableAcc float64
	for _, b := range bb {
		net += b.power
		if b.soc != nil {
			totalWeight += weight(b)
			usableAcc += b.usable() * weight(b)
		}
	}
	if totalWeight == 0 {
		return res
	}
	mean := usableAcc / totalWeight

	for i, b := range bb {
		if !b.controlled || b.soc == nil {
			continue
		}

		// forced modes own the device
		if b.mode == api.BatteryCharge || b.mode == api.BatteryDischarge {
			continue
		}

		u := b.usable()
		balance := max(-1, min(1, (u-mean)/dispatchBalanceSpread))

		// fair share of the net power, shifted towards the battery further from the mean
		p := net * weight(b) / totalWeight
		if net >= 0 {
			p *= 1 + balance
		} else {
			p *= 1 - balance
		}

		if b.mode == api.BatteryHoldDischarge || u <= 0 {
			p = min(p, 0)
		}
		if b.mode == api.BatteryHoldCharge || u >= 1 {
			p = max(p, 0)
		}
		if b.maxCharge > 0 {
			p = max(p, -b.maxCharge)
		}
		if b.maxDischarge > 0 {
			p = min(p, b.maxDischarge)
		}

		// too small to run the inverter, the self-regulating batteries take it
		if math.Abs(p) < b.minPower {
			p = 0
		}

		res[i] = new(p)
	}

	limitDischarge(bb, res, max(0, headroom))

	return res
}

// limitDischarge scales the discharging batteries down to the headroom. A battery pushed below its
// min power is stopped and the headroom is shared among the remaining ones.
func limitDischarge(bb []dispatchBattery, res []*float64, headroom float64) {
	var active []int
	for i, p := range res {
		if p != nil && *p > 0 {
			active = append(active, i)
		}
	}

	for len(active) > 0 {
		var sum float64
		for _, i := range active {
			sum += *res[i]
		}

		scale := min(1, headroom/sum)

		// stop the smallest battery that would fall below its min power and retry, ties stop the last one
		drop := -1
		for k, i := range active {
			if *res[i]*scale < bb[i].minPower && (drop < 0 || *res[i] <= *res[active[drop]]) {
				drop = k
			}
		}

		if drop < 0 {
			for _, i := range active {
				*res[i] *= scale
			}
			return
		}

		*res[active[drop]] = 0
		active = slices.Delete(active, drop, drop+1)
	}
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

		b := dispatchBattery{power: m.Power, soc: m.Soc, maxSoc: 100}
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

		site.log.DEBUG.Printf("battery %s power setpoint: %.0fW (headroom %.0fW)", deviceTitleOrName(dev), *p, headroom)
	}
}
