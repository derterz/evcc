package core

import (
	"testing"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/stretchr/testify/assert"
)

func TestDispatchBatteryPower(t *testing.T) {
	regulating := func(soc, pv float64) dispatchBattery {
		return dispatchBattery{mode: api.BatteryNormal, soc: new(soc), minSoc: 10, maxSoc: 100, capacity: 10, pv: pv}
	}
	controlled := func(soc, pv float64, mode api.BatteryMode) dispatchBattery {
		return dispatchBattery{controlled: true, mode: mode, soc: new(soc), minSoc: 10, maxSoc: 100, capacity: 5, maxCharge: 1500, maxDischarge: 800, pv: pv}
	}
	withMinPower := func(b dispatchBattery, p float64) dispatchBattery {
		b.minPower = p
		return b
	}
	running := func(b dispatchBattery) dispatchBattery {
		b.running = true
		return b
	}
	withMaxSoc := func(b dispatchBattery, soc float64) dispatchBattery {
		b.maxSoc = soc
		return b
	}
	normal := api.BatteryNormal

	for _, tc := range []struct {
		name   string
		bb     []dispatchBattery
		demand float64
		res    []*float64
	}{
		{
			"equal soc: discharge shared by capacity",
			[]dispatchBattery{regulating(55, 0), controlled(55, 0, normal)},
			900,
			[]*float64{nil, new(300.0)},
		},
		{
			"equal soc: charge from the self-regulating battery's pv",
			[]dispatchBattery{regulating(55, 1000), controlled(55, 0, normal)},
			-600,
			[]*float64{nil, new(-200.0)},
		},
		{
			"no demand",
			[]dispatchBattery{regulating(55, 0), controlled(55, 0, normal)},
			0,
			[]*float64{nil, new(0.0)},
		},
		{
			"controlled fuller: discharges more",
			// scaled soc 0.5 and 0.6, mean 0.533: factors 6.67 and 8.33
			[]dispatchBattery{regulating(55, 0), controlled(64, 0, normal)},
			900,
			[]*float64{nil, new(500.0)},
		},
		{
			"controlled emptier: no discharge",
			[]dispatchBattery{regulating(64, 0), controlled(46, 0, normal)},
			900,
			[]*float64{nil, new(0.0)},
		},
		{
			"controlled emptier: charges more",
			// factors 3.33 and 10
			[]dispatchBattery{regulating(64, 1500), controlled(46, 0, normal)},
			-1200,
			[]*float64{nil, new(-900.0)},
		},
		{
			"self-regulating empty: controlled covers the demand",
			[]dispatchBattery{regulating(10, 0), controlled(65, 0, normal)},
			900,
			[]*float64{nil, new(800.0)}, // max discharge
		},
		{
			"self-regulating full: controlled takes the surplus",
			[]dispatchBattery{regulating(100, 1500), controlled(50, 0, normal)},
			-1200,
			[]*float64{nil, new(-1200.0)},
		},
		{
			"self-regulating nearly full: controlled charges at max power",
			[]dispatchBattery{regulating(95, 3500), controlled(50, 0, normal)},
			-3000,
			[]*float64{nil, new(-1500.0)},
		},
		{
			"own pv beyond consumption is stored",
			// the self-regulating battery's share of the surplus would be AC charging
			[]dispatchBattery{regulating(55, 0), controlled(55, 1000, normal)},
			-700,
			[]*float64{nil, new(300.0)},
		},
		{
			"own pv beyond max discharge is stored",
			[]dispatchBattery{regulating(55, 0), controlled(55, 1000, normal)},
			300,
			[]*float64{nil, new(800.0)},
		},
		{
			"holddischarge passes own pv through",
			[]dispatchBattery{regulating(50, 0), controlled(65, 200, api.BatteryHoldDischarge)},
			700,
			[]*float64{nil, new(200.0)},
		},
		{
			"holddischarge allows charge",
			[]dispatchBattery{regulating(64, 1500), controlled(46, 0, api.BatteryHoldDischarge)},
			-1200,
			[]*float64{nil, new(-900.0)},
		},
		{
			"holdcharge passes own pv through",
			[]dispatchBattery{regulating(64, 1600), controlled(46, 400, api.BatteryHoldCharge)},
			-1200,
			[]*float64{nil, new(400.0)},
		},
		{
			"unknown mode treated as normal",
			[]dispatchBattery{regulating(55, 0), controlled(55, 0, api.BatteryUnknown)},
			900,
			[]*float64{nil, new(300.0)},
		},
		{
			"forced charge owns the device",
			[]dispatchBattery{regulating(55, 0), controlled(55, 0, api.BatteryCharge)},
			900,
			[]*float64{nil, nil},
		},
		{
			"forced discharge owns the device",
			[]dispatchBattery{regulating(55, 0), controlled(55, 0, api.BatteryDischarge)},
			900,
			[]*float64{nil, nil},
		},
		{
			"missing soc: not dispatched",
			[]dispatchBattery{regulating(55, 0), {controlled: true, mode: normal, capacity: 5}},
			900,
			[]*float64{nil, nil},
		},
		{
			"missing capacity: equal weights",
			[]dispatchBattery{{mode: normal, soc: new(55.0), minSoc: 10, maxSoc: 100}, controlled(55, 0, normal)},
			900,
			[]*float64{nil, new(450.0)},
		},
		{
			"scaled soc respects different limits",
			// 55% of 10..100 and 60% of 20..100 are both 0.5
			[]dispatchBattery{regulating(55, 0), {controlled: true, mode: normal, soc: new(60.0), minSoc: 20, maxSoc: 100, capacity: 5}},
			900,
			[]*float64{nil, new(300.0)},
		},
		{
			"at max soc: own pv passed through",
			[]dispatchBattery{regulating(90, 0), withMaxSoc(controlled(90, 300, normal), 90)},
			-100,
			[]*float64{nil, new(300.0)},
		},
		{
			"above max soc: discharged on top of curtailed pv",
			[]dispatchBattery{regulating(95, 0), withMaxSoc(controlled(95, 300, normal), 90)},
			0,
			[]*float64{nil, new(700.0)}, // 300 pv + half of max discharge
		},
		{
			"full: discharged at max power even if exported",
			[]dispatchBattery{regulating(100, 2000), controlled(100, 0, normal)},
			-900,
			[]*float64{nil, new(800.0)},
		},
		{
			"full in holddischarge: own pv passed through only",
			[]dispatchBattery{regulating(100, 0), controlled(100, 300, api.BatteryHoldDischarge)},
			0,
			[]*float64{nil, new(300.0)},
		},
		{
			"below min power: stopped",
			[]dispatchBattery{regulating(55, 0), withMinPower(controlled(55, 0, normal), 100)},
			200,
			[]*float64{nil, new(0.0)},
		},
		{
			"charge below min power: stopped",
			[]dispatchBattery{regulating(55, 500), withMinPower(controlled(55, 0, normal), 100)},
			-200,
			[]*float64{nil, new(0.0)},
		},
		{
			"running: kept down to min power",
			[]dispatchBattery{regulating(55, 0), running(withMinPower(controlled(55, 0, normal), 100))},
			360,
			[]*float64{nil, new(120.0)},
		},
		{
			"stopped: not started below start power",
			[]dispatchBattery{regulating(55, 0), withMinPower(controlled(55, 0, normal), 100)},
			360,
			[]*float64{nil, new(0.0)},
		},
		{
			"stopped: started above start power",
			[]dispatchBattery{regulating(55, 0), withMinPower(controlled(55, 0, normal), 100)},
			450,
			[]*float64{nil, new(150.0)},
		},
		{
			"three batteries: small demand runs one inverter",
			// 92 each would both be below start power
			[]dispatchBattery{regulating(50, 0), withMinPower(controlled(65, 0, normal), 100), withMinPower(controlled(65, 0, normal), 100)},
			200,
			[]*float64{nil, new(0.0), new(169.2)},
		},
		{
			"three batteries: running inverter keeps running",
			[]dispatchBattery{regulating(50, 0), running(withMinPower(controlled(65, 0, normal), 100)), withMinPower(controlled(65, 0, normal), 100)},
			150,
			[]*float64{nil, new(126.9), new(0.0)},
		},
		{
			"three batteries: equal soc shared",
			[]dispatchBattery{regulating(55, 0), withMinPower(controlled(55, 0, normal), 100), withMinPower(controlled(55, 0, normal), 100)},
			1200,
			[]*float64{nil, new(300.0), new(300.0)},
		},
		{
			"three batteries: beyond max power to the others",
			// scaled soc 0.5, 0.6 and 0.4, the fuller controlled battery's share of 1000 exceeds its max discharge
			[]dispatchBattery{regulating(55, 0), controlled(64, 0, normal), controlled(46, 0, normal)},
			2000,
			[]*float64{nil, new(800.0), new(0.0)},
		},
		{
			"three batteries: emptier charges more",
			[]dispatchBattery{regulating(55, 2000), controlled(54, 0, normal), controlled(56, 0, normal)},
			-1200,
			[]*float64{nil, new(-333.3), new(-266.7)},
		},
		{
			"three batteries: held battery's share goes to the others",
			[]dispatchBattery{regulating(55, 0), controlled(60, 0, api.BatteryHoldDischarge), controlled(55, 0, normal)},
			1200,
			[]*float64{nil, new(0.0), new(400.0)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := dispatchBatteryPower(tc.bb, tc.demand)
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

func TestDispatchDemand(t *testing.T) {
	record := func(vals ...float64) *Site {
		site := &Site{}
		now := time.Now()
		for i, p := range vals {
			site.recordDemand(now.Add(time.Duration(i)*30*time.Second), p)
		}
		return site
	}

	assert.Zero(t, (&Site{}).dispatchDemand())

	// samples older than the window are dropped
	site := record(900, 300, 2500, 400, 350, 800, 600, 500, 700, 450, 3000, 650)
	assert.Len(t, site.demandWindow, 11)

	// deficit: low percentile, short load peaks do not raise it
	assert.Equal(t, 350.0, site.dispatchDemand())

	// surplus: high percentile, short pv peaks do not raise it
	site = record(-800, -1200, -300, -900, -1000, -700, -1100, -600, -950, -850, -750)
	assert.Equal(t, -600.0, site.dispatchDemand())

	// fluctuating around zero
	site = record(-800, -500, -300, 200, 400, 600, -100, 300, -200, 500, 100)
	assert.Zero(t, site.dispatchDemand())
}
