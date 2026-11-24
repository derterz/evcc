package core

import (
	"math"
	"testing"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/stretchr/testify/assert"
)

func TestDispatchBatteryPower(t *testing.T) {
	regulating := func(soc, pv float64) dispatchRegulating {
		return sumRegulating([]dispatchBattery{{mode: api.BatteryNormal, soc: new(soc), minSoc: 10, maxSoc: 100, capacity: 10}}, pv)
	}
	controlled := func(soc, pv float64, mode api.BatteryMode) dispatchBattery {
		return dispatchBattery{mode: mode, soc: new(soc), minSoc: 10, maxSoc: 100, capacity: 5, maxCharge: 1500, maxDischarge: 800, pv: pv}
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
		reg    dispatchRegulating
		bb     []dispatchBattery
		demand float64
		res    []*float64
	}{
		{
			"equal soc: discharge shared by capacity",
			regulating(55, 0), []dispatchBattery{controlled(55, 0, normal)},
			900,
			[]*float64{new(300.0)},
		},
		{
			"equal soc: charge from the self-regulating battery's pv",
			regulating(55, 1000), []dispatchBattery{controlled(55, 0, normal)},
			-600,
			[]*float64{new(-200.0)},
		},
		{
			"no demand",
			regulating(55, 0), []dispatchBattery{controlled(55, 0, normal)},
			0,
			[]*float64{new(0.0)},
		},
		{
			"controlled fuller: discharges more",
			// scaled soc 0.5 and 0.6, mean 0.533: factors 6.67 and 8.33
			regulating(55, 0), []dispatchBattery{controlled(64, 0, normal)},
			900,
			[]*float64{new(500.0)},
		},
		{
			"controlled emptier: no discharge",
			regulating(64, 0), []dispatchBattery{controlled(46, 0, normal)},
			900,
			[]*float64{new(0.0)},
		},
		{
			"controlled emptier: charges more",
			// factors 3.33 and 10
			regulating(64, 1500), []dispatchBattery{controlled(46, 0, normal)},
			-1200,
			[]*float64{new(-900.0)},
		},
		{
			"self-regulating output limited: controlled takes the rest",
			sumRegulating([]dispatchBattery{{mode: normal, soc: new(55.0), minSoc: 10, maxSoc: 100, capacity: 10, maxDischarge: 500}}, 0),
			[]dispatchBattery{controlled(55, 0, normal)},
			900,
			[]*float64{new(400.0)},
		},
		{
			"self-regulating empty: controlled covers the demand",
			regulating(10, 0), []dispatchBattery{controlled(65, 0, normal)},
			900,
			[]*float64{new(800.0)}, // max discharge
		},
		{
			"self-regulating full: controlled takes the surplus",
			regulating(100, 1500), []dispatchBattery{controlled(50, 0, normal)},
			-1200,
			[]*float64{new(-1200.0)},
		},
		{
			"self-regulating nearly full: controlled charges at max power",
			regulating(95, 3500), []dispatchBattery{controlled(50, 0, normal)},
			-3000,
			[]*float64{new(-1500.0)},
		},
		{
			"own pv beyond consumption is stored",
			// the self-regulating battery's share of the surplus would be AC charging
			regulating(55, 0), []dispatchBattery{controlled(55, 1000, normal)},
			-700,
			[]*float64{new(300.0)},
		},
		{
			"own pv beyond max discharge is stored",
			regulating(55, 0), []dispatchBattery{controlled(55, 1000, normal)},
			300,
			[]*float64{new(800.0)},
		},
		{
			"holddischarge passes own pv through",
			regulating(50, 0), []dispatchBattery{controlled(65, 200, api.BatteryHoldDischarge)},
			700,
			[]*float64{new(200.0)},
		},
		{
			"holddischarge allows charge",
			regulating(64, 1500), []dispatchBattery{controlled(46, 0, api.BatteryHoldDischarge)},
			-1200,
			[]*float64{new(-900.0)},
		},
		{
			"holdcharge passes own pv through",
			regulating(64, 1600), []dispatchBattery{controlled(46, 400, api.BatteryHoldCharge)},
			-1200,
			[]*float64{new(400.0)},
		},
		{
			"unknown mode treated as normal",
			regulating(55, 0), []dispatchBattery{controlled(55, 0, api.BatteryUnknown)},
			900,
			[]*float64{new(300.0)},
		},
		{
			"forced charge owns the device",
			regulating(55, 0), []dispatchBattery{controlled(55, 0, api.BatteryCharge)},
			900,
			[]*float64{nil},
		},
		{
			"forced discharge owns the device",
			regulating(55, 0), []dispatchBattery{controlled(55, 0, api.BatteryDischarge)},
			900,
			[]*float64{nil},
		},
		{
			"missing soc: not dispatched",
			regulating(55, 0), []dispatchBattery{{mode: normal, capacity: 5}},
			900,
			[]*float64{nil},
		},
		{
			"missing capacity: equal weights",
			sumRegulating([]dispatchBattery{{mode: normal, soc: new(55.0), minSoc: 10, maxSoc: 100}}, 0), []dispatchBattery{controlled(55, 0, normal)},
			900,
			[]*float64{new(450.0)},
		},
		{
			"scaled soc respects different limits",
			// 55% of 10..100 and 60% of 20..100 are both 0.5
			regulating(55, 0), []dispatchBattery{{mode: normal, soc: new(60.0), minSoc: 20, maxSoc: 100, capacity: 5}},
			900,
			[]*float64{new(300.0)},
		},
		{
			"at max soc: own pv passed through",
			regulating(90, 0), []dispatchBattery{withMaxSoc(controlled(90, 300, normal), 90)},
			-100,
			[]*float64{new(300.0)},
		},
		{
			"above max soc: discharged on top of curtailed pv",
			regulating(95, 0), []dispatchBattery{withMaxSoc(controlled(95, 300, normal), 90)},
			0,
			[]*float64{new(700.0)}, // 300 pv + half of max discharge
		},
		{
			"full: discharged at max power even if exported",
			regulating(100, 2000), []dispatchBattery{controlled(100, 0, normal)},
			-900,
			[]*float64{new(800.0)},
		},
		{
			"full in holddischarge: own pv passed through only",
			regulating(100, 0), []dispatchBattery{controlled(100, 300, api.BatteryHoldDischarge)},
			0,
			[]*float64{new(300.0)},
		},
		{
			"below min power: stopped",
			regulating(55, 0), []dispatchBattery{withMinPower(controlled(55, 0, normal), 100)},
			200,
			[]*float64{new(0.0)},
		},
		{
			"charge below min power: stopped",
			regulating(55, 500), []dispatchBattery{withMinPower(controlled(55, 0, normal), 100)},
			-200,
			[]*float64{new(0.0)},
		},
		{
			"running: kept down to min power",
			regulating(55, 0), []dispatchBattery{running(withMinPower(controlled(55, 0, normal), 100))},
			360,
			[]*float64{new(120.0)},
		},
		{
			"stopped: not started below start power",
			regulating(55, 0), []dispatchBattery{withMinPower(controlled(55, 0, normal), 100)},
			360,
			[]*float64{new(0.0)},
		},
		{
			"stopped: started above start power",
			regulating(55, 0), []dispatchBattery{withMinPower(controlled(55, 0, normal), 100)},
			450,
			[]*float64{new(150.0)},
		},
		{
			"three batteries: small demand runs one inverter",
			// 92 each would both be below start power
			regulating(50, 0), []dispatchBattery{withMinPower(controlled(65, 0, normal), 100), withMinPower(controlled(65, 0, normal), 100)},
			200,
			[]*float64{new(0.0), new(169.2)},
		},
		{
			"three batteries: running inverter keeps running",
			regulating(50, 0), []dispatchBattery{running(withMinPower(controlled(65, 0, normal), 100)), withMinPower(controlled(65, 0, normal), 100)},
			150,
			[]*float64{new(126.9), new(0.0)},
		},
		{
			"three batteries: equal soc shared",
			regulating(55, 0), []dispatchBattery{withMinPower(controlled(55, 0, normal), 100), withMinPower(controlled(55, 0, normal), 100)},
			1200,
			[]*float64{new(300.0), new(300.0)},
		},
		{
			"three batteries: beyond max power to the others",
			// scaled soc 0.5, 0.6 and 0.4, the fuller controlled battery's share of 1000 exceeds its max discharge
			regulating(55, 0), []dispatchBattery{controlled(64, 0, normal), controlled(46, 0, normal)},
			2000,
			[]*float64{new(800.0), new(0.0)},
		},
		{
			"three batteries: emptier charges more",
			regulating(55, 2000), []dispatchBattery{controlled(54, 0, normal), controlled(56, 0, normal)},
			-1200,
			[]*float64{new(-333.3), new(-266.7)},
		},
		{
			"three batteries: held battery's share goes to the others",
			regulating(55, 0), []dispatchBattery{controlled(60, 0, api.BatteryHoldDischarge), controlled(55, 0, normal)},
			1200,
			[]*float64{new(0.0), new(400.0)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := dispatchBatteryPower(tc.reg, tc.bb, tc.demand)
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

func TestSumRegulating(t *testing.T) {
	battery := func(soc, capacity, maxDischarge float64, mode api.BatteryMode) dispatchBattery {
		return dispatchBattery{mode: mode, soc: new(soc), minSoc: 10, maxSoc: 100, capacity: capacity, maxDischarge: maxDischarge}
	}
	normal := api.BatteryNormal

	// weighted by capacity, empty and forced batteries add no output
	r := sumRegulating([]dispatchBattery{
		battery(55, 10, 3000, normal),
		battery(10, 5, 2000, normal),
		battery(50, 5, 2000, api.BatteryCharge),
	}, 500)
	assert.Equal(t, 15.0, r.capacity)
	assert.InDelta(t, 1.0/3, *r.scaledSoc, 1e-9)
	assert.Equal(t, 3000.0, r.output)
	assert.False(t, r.full)
	assert.Equal(t, 500.0, r.pv)

	// unknown capacity: equal weights, unknown max discharge: unlimited
	r = sumRegulating([]dispatchBattery{
		battery(55, 0, 0, normal),
		battery(100, 10, 2000, normal),
	}, 0)
	assert.Zero(t, r.capacity)
	assert.InDelta(t, 0.75, *r.scaledSoc, 1e-9)
	assert.True(t, math.IsInf(r.output, 1))
	assert.False(t, r.full)

	// none can charge
	r = sumRegulating([]dispatchBattery{
		battery(100, 10, 2000, normal),
		battery(50, 10, 2000, api.BatteryHoldCharge),
	}, 0)
	assert.True(t, r.full)

	// no soc
	r = sumRegulating([]dispatchBattery{{mode: normal, capacity: 10}}, 0)
	assert.Nil(t, r.scaledSoc)
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
