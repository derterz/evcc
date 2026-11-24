package core

import (
	"math"
	"slices"
	"testing"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/types"
	"github.com/stretchr/testify/assert"
)

// measurement returns a battery measurement, a capacity of 0 is unknown
func measurement(soc, minSoc, maxSoc, capacity float64) types.Measurement {
	m := types.Measurement{Soc: &soc, MinSoc: &minSoc, MaxSoc: &maxSoc}
	if capacity > 0 {
		m.Capacity = &capacity
	}
	return m
}

func TestDispatchBatteryPower(t *testing.T) {
	normal := api.BatteryNormal

	// the self-regulating battery's pv passes the inverter in addition to its discharge
	regulating := func(soc, pv float64) dispatchBattery {
		return newDispatchBattery(measurement(soc, 10, 100, 10), normal, 1, 0, 5000+pv, pv)
	}
	controlled := func(soc, pv float64, mode api.BatteryMode) dispatchBattery {
		return newDispatchBattery(measurement(soc, 10, 100, 5), mode, 1, 1500, 800, pv)
	}
	controlledMaxSoc90 := func(soc, pv float64) dispatchBattery {
		return newDispatchBattery(measurement(soc, 10, 90, 5), normal, 1, 1500, 800, pv)
	}
	running := func(b dispatchBattery) dispatchBattery {
		b.running = true
		return b
	}
	withMinPower := func(b dispatchBattery, p float64) dispatchBattery {
		b.minPower = p
		return b
	}

	for _, tc := range []struct {
		name   string
		reg    dispatchBattery
		bb     []dispatchBattery
		demand float64
		res    []float64
	}{
		{"equal soc: discharge shared by capacity", regulating(55, 0), []dispatchBattery{controlled(55, 0, normal)}, 900, []float64{300}},
		{"equal soc: charge from the self-regulating battery's pv", regulating(55, 1000), []dispatchBattery{controlled(55, 0, normal)}, -600, []float64{-200}},
		{"no demand", regulating(55, 0), []dispatchBattery{controlled(55, 0, normal)}, 0, []float64{0}},
		// scaled soc 0.5 and 0.6, mean 0.533: factors 6.67 and 8.33
		{"controlled fuller: discharges more", regulating(55, 0), []dispatchBattery{controlled(64, 0, normal)}, 900, []float64{500}},
		{"controlled emptier: no discharge", regulating(64, 0), []dispatchBattery{controlled(46, 0, normal)}, 900, []float64{0}},
		// factors 3.33 and 10
		{"controlled emptier: charges more", regulating(64, 1500), []dispatchBattery{controlled(46, 0, normal)}, -1200, []float64{-900}},
		{
			"self-regulating output limited: controlled takes the rest",
			newDispatchBattery(measurement(55, 10, 100, 10), normal, 1, 0, 500, 0),
			[]dispatchBattery{controlled(55, 0, normal)}, 900, []float64{400},
		},
		{"self-regulating empty: controlled covers the demand", regulating(10, 0), []dispatchBattery{controlled(65, 0, normal)}, 900, []float64{800}},
		{"self-regulating at max soc: controlled takes the surplus", regulating(99, 1500), []dispatchBattery{controlled(50, 0, normal)}, -1200, []float64{-1200}},
		// at 100% the self-regulating battery is expected to discharge at max power
		{"self-regulating at 100%: controlled charges at max power", regulating(100, 1500), []dispatchBattery{controlled(50, 0, normal)}, -1200, []float64{-1500}},
		{
			"self-regulating in holdcharge: controlled takes the surplus",
			newDispatchBattery(measurement(50, 10, 100, 10), api.BatteryHoldCharge, 1, 0, 7000, 2000),
			[]dispatchBattery{controlled(50, 0, normal)}, -1500, []float64{-1500},
		},
		{"self-regulating nearly full: controlled charges at max power", regulating(95, 3500), []dispatchBattery{controlled(50, 0, normal)}, -3000, []float64{-1500}},
		// the self-regulating battery's share of the surplus would be AC charging
		{"own pv beyond consumption is stored", regulating(55, 0), []dispatchBattery{controlled(55, 1000, normal)}, -700, []float64{300}},
		{"own pv beyond max discharge is stored", regulating(55, 0), []dispatchBattery{controlled(55, 1000, normal)}, 300, []float64{800}},
		{"holddischarge passes own pv through", regulating(50, 0), []dispatchBattery{controlled(65, 200, api.BatteryHoldDischarge)}, 700, []float64{200}},
		{"holddischarge allows charge", regulating(64, 1500), []dispatchBattery{controlled(46, 0, api.BatteryHoldDischarge)}, -1200, []float64{-900}},
		{"holdcharge passes own pv through", regulating(64, 1600), []dispatchBattery{controlled(46, 400, api.BatteryHoldCharge)}, -1200, []float64{400}},
		{"unknown mode treated as normal", regulating(55, 0), []dispatchBattery{controlled(55, 0, api.BatteryUnknown)}, 900, []float64{300}},
		{
			"assumed capacity: equal weights",
			newDispatchBattery(measurement(55, 10, 100, 0), normal, 5, 0, 5000, 0),
			[]dispatchBattery{controlled(55, 0, normal)}, 900, []float64{450},
		},
		{
			// 55% of 10..100 and 60% of 20..100 are both 0.5, usable capacities 9 and 4 kWh
			"usable capacity respects different limits",
			regulating(55, 0),
			[]dispatchBattery{newDispatchBattery(measurement(60, 20, 100, 5), normal, 1, 1500, 800, 0)}, 900, []float64{276.9},
		},
		{"at max soc: own pv passed through", regulating(90, 0), []dispatchBattery{controlledMaxSoc90(90, 300)}, -100, []float64{300}},
		// 300 pv + half of max discharge
		{"above max soc: discharged on top of curtailed pv", regulating(95, 0), []dispatchBattery{controlledMaxSoc90(95, 300)}, 0, []float64{700}},
		{"full: discharged at max power even if exported", regulating(100, 2000), []dispatchBattery{controlled(100, 0, normal)}, -900, []float64{800}},
		{"full in holddischarge: own pv passed through only", regulating(100, 0), []dispatchBattery{controlled(100, 300, api.BatteryHoldDischarge)}, 0, []float64{300}},
		{"below min power: stopped", regulating(55, 0), []dispatchBattery{withMinPower(controlled(55, 0, normal), 100)}, 200, []float64{0}},
		{"charge below min power: stopped", regulating(55, 500), []dispatchBattery{withMinPower(controlled(55, 0, normal), 100)}, -200, []float64{0}},
		{"running: kept down to min power", regulating(55, 0), []dispatchBattery{running(withMinPower(controlled(55, 0, normal), 100))}, 360, []float64{120}},
		{"stopped: not started below start power", regulating(55, 0), []dispatchBattery{withMinPower(controlled(55, 0, normal), 100)}, 360, []float64{0}},
		{"stopped: started above start power", regulating(55, 0), []dispatchBattery{withMinPower(controlled(55, 0, normal), 100)}, 450, []float64{150}},
		{
			// 92 each would both be below start power
			"three batteries: small demand runs one inverter",
			regulating(50, 0),
			[]dispatchBattery{withMinPower(controlled(65, 0, normal), 100), withMinPower(controlled(65, 0, normal), 100)}, 200, []float64{0, 169.2},
		},
		{
			"three batteries: running inverter keeps running",
			regulating(50, 0),
			[]dispatchBattery{running(withMinPower(controlled(65, 0, normal), 100)), withMinPower(controlled(65, 0, normal), 100)}, 150, []float64{126.9, 0},
		},
		{
			"three batteries: equal soc shared",
			regulating(55, 0),
			[]dispatchBattery{withMinPower(controlled(55, 0, normal), 100), withMinPower(controlled(55, 0, normal), 100)}, 1200, []float64{300, 300},
		},
		{
			// scaled soc 0.5, 0.6 and 0.4, the fuller controlled battery's share of 1000 exceeds its max discharge
			"three batteries: beyond max power to the others",
			regulating(55, 0),
			[]dispatchBattery{controlled(64, 0, normal), controlled(46, 0, normal)}, 2000, []float64{800, 0},
		},
		{
			"three batteries: emptier charges more",
			regulating(55, 2000),
			[]dispatchBattery{controlled(54, 0, normal), controlled(56, 0, normal)}, -1200, []float64{-333.3, -266.7},
		},
		{
			"three batteries: held battery's share goes to the others",
			regulating(55, 0),
			[]dispatchBattery{controlled(60, 0, api.BatteryHoldDischarge), controlled(55, 0, normal)}, 1200, []float64{0, 400},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dispatched := append(slices.Clone(tc.bb), sumRegulating([]dispatchBattery{tc.reg}))

			res := dispatchBatteryPower(dispatched, tc.demand)
			for i := range tc.res {
				assert.InDelta(t, tc.res[i], res[i], 1, "battery %d", i)
			}
		})
	}
}

func TestNewDispatchBattery(t *testing.T) {
	controlled := func(soc, pv float64, mode api.BatteryMode) dispatchBattery {
		return newDispatchBattery(measurement(soc, 10, 90, 5), mode, 1, 1500, 800, pv)
	}

	for _, tc := range []struct {
		name                    string
		b                       dispatchBattery
		scaled, over, under     float64
		minAC, maxAC, preferred float64
	}{
		{"normal", controlled(50, 200, api.BatteryNormal), 0.5, 0, 0, -1500, 800, 200},
		{"holddischarge passes pv", controlled(50, 200, api.BatteryHoldDischarge), 0.5, 0, 0, -1500, 200, 200},
		{"holdcharge passes pv", controlled(50, 200, api.BatteryHoldCharge), 0.5, 0, 0, 200, 800, 200},
		{"below min soc passes pv", controlled(5, 200, api.BatteryNormal), -0.0625, 0, 0.5, -1500, 200, 200},
		{"above max soc discharges", controlled(95, 300, api.BatteryNormal), 1.0625, 0.5, 0, 700, 800, 700},
		{"at 100% at max power", controlled(100, 300, api.BatteryNormal), 1.125, 1, 0, 800, 800, 800},
		{"pv beyond max discharge", controlled(50, 1000, api.BatteryNormal), 0.5, 0, 0, -1500, 800, 800},
		{"charges from own pv only", newDispatchBattery(measurement(50, 10, 90, 5), api.BatteryNormal, 1, 0, 800, 300), 0.5, 0, 0, 0, 800, 300},
		{"charges from own pv only, full", newDispatchBattery(measurement(95, 10, 90, 5), api.BatteryNormal, 1, 0, 800, 300), 1.0625, 0.5, 0, 700, 800, 700},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.b
			assert.InDelta(t, tc.scaled, b.scaledSoc, 1e-9, "scaled")
			assert.InDelta(t, tc.over, b.overcharged, 1e-9, "overcharged")
			assert.InDelta(t, tc.under, b.undercharged, 1e-9, "undercharged")
			assert.Equal(t, tc.minAC, b.minAC, "min")
			assert.Equal(t, tc.maxAC, b.maxAC, "max")
			assert.Equal(t, tc.preferred, b.preferred(), "preferred")
			assert.InDelta(t, 4.0, b.usableCapacity, 1e-9, "usable capacity")
			assert.False(t, b.capacityAssumed)
		})
	}

	// unknown capacity and soc limits
	b := newDispatchBattery(types.Measurement{Soc: new(50.0)}, api.BatteryNormal, 3, 1500, 800, 0)
	assert.True(t, b.capacityAssumed)
	assert.Equal(t, 3.0, b.usableCapacity)
	assert.Equal(t, 0.5, b.scaledSoc)
}

func TestAverageCapacity(t *testing.T) {
	assert.Equal(t, 5.0, averageCapacity([]types.Measurement{measurement(50, 0, 100, 0), measurement(50, 0, 100, 4), measurement(50, 0, 100, 6)}))
	assert.Equal(t, 1.0, averageCapacity([]types.Measurement{measurement(50, 0, 100, 0)}))
	assert.Equal(t, 4.0, averageCapacity([]types.Measurement{measurement(50, 0, 100, math.NaN()), measurement(50, 0, 100, math.Inf(1)), measurement(50, 0, 100, 4)}))
}

func TestNewDispatchBatteryInvalidValues(t *testing.T) {
	// invalid soc limits fall back to 0..100, an invalid capacity is assumed
	m := types.Measurement{Soc: new(50.0), MinSoc: new(math.NaN()), MaxSoc: new(90.0), Capacity: new(math.Inf(1))}
	b := newDispatchBattery(m, api.BatteryNormal, 3, 1500, 800, 0)
	assert.Equal(t, 0.5, b.scaledSoc)
	assert.True(t, b.capacityAssumed)
	assert.Equal(t, 3.0, b.usableCapacity)
}

func TestSumRegulating(t *testing.T) {
	battery := func(soc, capacity, maxDischarge, pv float64, mode api.BatteryMode) dispatchBattery {
		return newDispatchBattery(measurement(soc, 10, 100, capacity), mode, 1, 0, maxDischarge+pv, pv)
	}

	// weighted by usable capacity, empty batteries add no output
	r := sumRegulating([]dispatchBattery{
		battery(55, 10, 3000, 500, api.BatteryNormal),
		battery(10, 5, 2000, 0, api.BatteryNormal),
	})
	assert.InDelta(t, 13.5, r.usableCapacity, 1e-9)
	assert.InDelta(t, 1.0/3, r.scaledSoc, 1e-9)
	assert.Equal(t, 0.0, r.minAC)
	assert.Equal(t, 3500.0, r.maxAC)
	assert.Equal(t, 500.0, r.preferred())

	// none can charge: pv passed through, at 100% plus max discharge
	r = sumRegulating([]dispatchBattery{
		battery(100, 10, 2000, 300, api.BatteryNormal),
		battery(50, 10, 2000, 0, api.BatteryHoldCharge),
	})
	assert.Equal(t, 2300.0, r.minAC)
	assert.Equal(t, 4300.0, r.maxAC)
}

func TestDispatchTable(t *testing.T) {
	ctrl := newDispatchBattery(measurement(50, 10, 90, 0), api.BatteryNormal, 1, 800, 800, 200)
	ctrl.limitsAssumed = true
	reg := newDispatchBattery(measurement(60, 10, 100, 10), api.BatteryNormal, 1, 0, 6000, 1000)

	table := dispatchTable([]dispatchBattery{ctrl, reg, sumRegulating([]dispatchBattery{reg})})
	assert.Contains(t, table, "0.8*")  // assumed capacity, usable kWh
	assert.Contains(t, table, "-800*") // assumed limits
	assert.Contains(t, table, "self-regulating")
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

	// invalid samples are not recorded
	site := &Site{}
	site.recordDemand(time.Now(), math.NaN())
	site.recordDemand(time.Now(), math.Inf(-1))
	assert.Empty(t, site.demandWindow)

	// samples older than the window are dropped
	site = record(900, 300, 2500, 400, 350, 800, 600, 500, 700, 450, 3000, 650)
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
