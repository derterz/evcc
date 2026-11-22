package core

import (
	"testing"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/stretchr/testify/assert"
)

func TestDispatchBatteryPower(t *testing.T) {
	// kostal is self-regulating, marstek takes setpoints
	kostal := func(soc, power float64) dispatchBattery {
		return dispatchBattery{soc: new(soc), minSoc: 10, maxSoc: 100, capacity: 10, power: power}
	}
	marstek := func(soc, power float64, mode api.BatteryMode) dispatchBattery {
		return dispatchBattery{controlled: true, mode: mode, soc: new(soc), minSoc: 10, maxSoc: 100, capacity: 5, power: power, maxCharge: 1500, maxDischarge: 800}
	}

	for _, tc := range []struct {
		name     string
		bb       []dispatchBattery
		headroom float64
		res      []*float64
	}{
		{
			"equal soc discharge: share by capacity",
			[]dispatchBattery{kostal(55, 900), marstek(55, 0, api.BatteryNormal)},
			1000,
			[]*float64{nil, new(300.0)},
		},
		{
			"marstek fuller: discharges double share",
			[]dispatchBattery{kostal(50, 900), marstek(65, 0, api.BatteryNormal)},
			1000,
			[]*float64{nil, new(600.0)},
		},
		{
			"marstek emptier: no discharge",
			[]dispatchBattery{kostal(60, 900), marstek(45, 0, api.BatteryNormal)},
			1000,
			[]*float64{nil, new(0.0)},
		},
		{
			"discharge capped by headroom",
			[]dispatchBattery{kostal(50, 900), marstek(65, 0, api.BatteryNormal)},
			250,
			[]*float64{nil, new(250.0)},
		},
		{
			"no headroom",
			[]dispatchBattery{kostal(50, 900), marstek(65, 0, api.BatteryNormal)},
			-100,
			[]*float64{nil, new(0.0)},
		},
		{
			"marstek emptier: charges double share",
			[]dispatchBattery{kostal(60, -1200), marstek(45, 0, api.BatteryNormal)},
			1000,
			[]*float64{nil, new(-800.0)},
		},
		{
			"marstek fuller: no charge",
			[]dispatchBattery{kostal(50, -1200), marstek(65, 0, api.BatteryNormal)},
			1000,
			[]*float64{nil, new(0.0)},
		},
		{
			"charge limited",
			[]dispatchBattery{kostal(60, -3000), marstek(45, 0, api.BatteryNormal)},
			1000,
			[]*float64{nil, new(-1500.0)},
		},
		{
			"net includes the marstek's own power",
			[]dispatchBattery{kostal(55, 600), marstek(55, 300, api.BatteryNormal)},
			1000,
			[]*float64{nil, new(300.0)},
		},
		{
			"holddischarge blocks discharge",
			[]dispatchBattery{kostal(50, 900), marstek(65, 0, api.BatteryHoldDischarge)},
			1000,
			[]*float64{nil, new(0.0)},
		},
		{
			"holddischarge allows charge",
			[]dispatchBattery{kostal(60, -1200), marstek(45, 0, api.BatteryHoldDischarge)},
			1000,
			[]*float64{nil, new(-800.0)},
		},
		{
			"holdcharge blocks charge",
			[]dispatchBattery{kostal(60, -1200), marstek(45, 0, api.BatteryHoldCharge)},
			1000,
			[]*float64{nil, new(0.0)},
		},
		{
			"unknown mode treated as normal",
			[]dispatchBattery{kostal(55, 900), marstek(55, 0, api.BatteryUnknown)},
			1000,
			[]*float64{nil, new(300.0)},
		},
		{
			"forced charge owns the device",
			[]dispatchBattery{kostal(55, 900), marstek(55, 0, api.BatteryCharge)},
			1000,
			[]*float64{nil, nil},
		},
		{
			"forced discharge owns the device",
			[]dispatchBattery{kostal(55, 900), marstek(55, 0, api.BatteryDischarge)},
			1000,
			[]*float64{nil, nil},
		},
		{
			"at min soc: no discharge",
			[]dispatchBattery{kostal(10, 900), marstek(10, 0, api.BatteryNormal)},
			1000,
			[]*float64{nil, new(0.0)},
		},
		{
			"at max soc: no charge",
			[]dispatchBattery{kostal(100, -900), marstek(100, 0, api.BatteryNormal)},
			1000,
			[]*float64{nil, new(0.0)},
		},
		{
			"usable soc respects different limits",
			// kostal 55% of 10..100 = 0.5, marstek 60% of 20..100 = 0.5
			[]dispatchBattery{kostal(55, 900), {controlled: true, mode: api.BatteryNormal, soc: new(60.0), minSoc: 20, maxSoc: 100, capacity: 5}},
			1000,
			[]*float64{nil, new(300.0)},
		},
		{
			"missing soc: not dispatched",
			[]dispatchBattery{kostal(55, 900), {controlled: true, mode: api.BatteryNormal, capacity: 5}},
			1000,
			[]*float64{nil, nil},
		},
		{
			"three batteries: headroom shared proportionally",
			[]dispatchBattery{kostal(50, 1500), marstek(65, 0, api.BatteryNormal), marstek(65, 0, api.BatteryNormal)},
			600,
			// unconstrained 750 each, capped at 800, scaled to 600 total
			[]*float64{nil, new(300.0), new(300.0)},
		},
		{
			"three batteries: each balanced towards the mean",
			// mean usable 0.5, marstek 1 at +0.1, marstek 2 at -0.1
			[]dispatchBattery{kostal(55, 2000), marstek(64, 0, api.BatteryNormal), marstek(46, 0, api.BatteryNormal)},
			2000,
			[]*float64{nil, new(800.0), new(0.0)}, // 1000 capped at max discharge
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := dispatchBatteryPower(tc.bb, tc.headroom)
			assert.Len(t, res, len(tc.res))
			for i := range tc.res {
				if tc.res[i] == nil {
					assert.Nil(t, res[i], "battery %d", i)
					continue
				}
				if assert.NotNil(t, res[i], "battery %d", i) {
					assert.InDelta(t, *tc.res[i], *res[i], 1, "battery %d", i)
				}
			}
		})
	}
}

func TestHomePowerPercentile(t *testing.T) {
	site := &Site{}
	assert.Zero(t, site.homePowerPercentile(dispatchPercentile))

	now := time.Now()
	for i, p := range []float64{900, 300, 2500, 400, 350, 800, 600, 500, 700, 450, 3000} {
		site.recordHomePower(now.Add(time.Duration(i)*30*time.Second), p)
	}

	// samples older than the window were dropped
	assert.Len(t, site.homePowerWindow, 11)
	site.recordHomePower(now.Add(11*30*time.Second), 650)
	assert.Len(t, site.homePowerWindow, 11)

	// the short load peaks do not raise the headroom
	assert.Equal(t, 350.0, site.homePowerPercentile(dispatchPercentile))
}
