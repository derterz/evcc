package meter

import (
	"context"
	"fmt"
	"math"

	"github.com/evcc-io/evcc/plugin"
)

// batteryPowerCtx configures an api.BatteryPowerController from separate charge and discharge setters
type batteryPowerCtx struct {
	ChargePower    *plugin.Config // AC charge power in W, 0 stops
	DischargePower *plugin.Config // AC discharge power in W, 0 stops
	PvPower        *plugin.Config // optional, the device's own PV power that passes through its AC side
	MinPower       float64        // AC setpoints below are sent as stop to save inverter standby losses
}

// Controller returns the pv power getter, min power getter and AC power setter, nil if not configured.
// limits are the AC power limits used for clamping, may be nil.
func (cc *batteryPowerCtx) Controller(ctx context.Context, limits func() (float64, float64, error)) (func() (float64, error), func() float64, func(float64) error, error) {
	if cc.ChargePower == nil && cc.DischargePower == nil {
		return nil, nil, nil, nil
	}
	if cc.ChargePower == nil || cc.DischargePower == nil {
		return nil, nil, nil, fmt.Errorf("battery power: chargePower and dischargePower must both be configured")
	}

	chargeS, err := cc.ChargePower.IntSetter(ctx, "chargePower")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("battery charge power: %w", err)
	}

	dischargeS, err := cc.DischargePower.IntSetter(ctx, "dischargePower")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("battery discharge power: %w", err)
	}

	pvG, err := cc.PvPower.FloatGetter(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("battery pv power: %w", err)
	}
	if pvG == nil {
		pvG = func() (float64, error) { return 0, nil }
	}

	minPower := cc.MinPower
	minG := func() float64 { return minPower }

	return pvG, minG, func(power float64) error {
		var maxCharge, maxDischarge float64
		if limits != nil {
			if c, d, err := limits(); err == nil {
				maxCharge, maxDischarge = c, d
			}
		}

		ac := batteryACPower(power, maxCharge, maxDischarge, minPower)

		if ac < 0 {
			return chargeS(int64(-ac))
		}
		return dischargeS(int64(ac))
	}, nil
}

// batteryACPower clamps an AC setpoint to the limits, 0 means unlimited. Setpoints below minPower stop the inverter.
func batteryACPower(ac, maxCharge, maxDischarge, minPower float64) float64 {
	if maxCharge > 0 {
		ac = max(ac, -maxCharge)
	}
	if maxDischarge > 0 {
		ac = min(ac, maxDischarge)
	}

	ac = math.Round(ac)
	if math.Abs(ac) < minPower {
		return 0
	}

	return ac
}
