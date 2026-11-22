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

// Controller returns the battery power setter, nil if not configured.
// limits are the AC power limits used for clamping, may be nil.
func (cc *batteryPowerCtx) Controller(ctx context.Context, limits func() (float64, float64, error)) (func(float64) error, error) {
	if cc.ChargePower == nil && cc.DischargePower == nil {
		return nil, nil
	}
	if cc.ChargePower == nil || cc.DischargePower == nil {
		return nil, fmt.Errorf("battery power: chargePower and dischargePower must both be configured")
	}

	chargeS, err := cc.ChargePower.IntSetter(ctx, "chargePower")
	if err != nil {
		return nil, fmt.Errorf("battery charge power: %w", err)
	}

	dischargeS, err := cc.DischargePower.IntSetter(ctx, "dischargePower")
	if err != nil {
		return nil, fmt.Errorf("battery discharge power: %w", err)
	}

	pvG, err := cc.PvPower.FloatGetter(ctx)
	if err != nil {
		return nil, fmt.Errorf("battery pv power: %w", err)
	}

	minPower := cc.MinPower

	return func(power float64) error {
		var pv, maxCharge, maxDischarge float64

		if pvG != nil {
			var err error
			if pv, err = pvG(); err != nil {
				return fmt.Errorf("pv power: %w", err)
			}
		}

		if limits != nil {
			if c, d, err := limits(); err == nil {
				maxCharge, maxDischarge = c, d
			}
		}

		ac := batteryACPower(power, pv, maxCharge, maxDischarge, minPower)

		if ac < 0 {
			return chargeS(int64(-ac))
		}
		return dischargeS(int64(ac))
	}, nil
}

// batteryACPower converts a battery side setpoint to the AC side by adding the own PV power that passes through.
// Limits of 0 mean unlimited, setpoints below minPower stop the inverter.
func batteryACPower(power, pv, maxCharge, maxDischarge, minPower float64) float64 {
	ac := power + max(0, pv)

	if maxCharge > 0 {
		ac = max(ac, -maxCharge)
	}
	if maxDischarge > 0 {
		ac = min(ac, maxDischarge)
	}

	ac = math.Round(ac/10) * 10
	if math.Abs(ac) < minPower {
		return 0
	}

	return ac
}
