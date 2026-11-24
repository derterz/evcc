package core

import (
	"slices"
	"testing"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/stretchr/testify/assert"
)

func TestDispatchBatteryPower(t *testing.T) {
	regulating := func(soc float64) dispatchBattery {
		return dispatchBattery{mode: api.BatteryNormal, soc: soc, minSoc: 10, maxSoc: 100, capacity: 10, maxCharge: 5000, maxDischarge: 5000}
	}
	controlled := func(soc, pv float64, mode api.BatteryMode) dispatchBattery {
		return dispatchBattery{controlled: true, mode: mode, soc: soc, minSoc: 10, maxSoc: 100, capacity: 5, maxCharge: 1500, maxDischarge: 800, pv: pv}
	}
	running := func(b dispatchBattery) dispatchBattery {
		b.running = true
		return b
	}
	withMinPower := func(b dispatchBattery, p float64) dispatchBattery {
		b.minPower = p
		return b
	}
	withMaxSoc := func(b dispatchBattery, soc float64) dispatchBattery {
		b.maxSoc = soc
		return b
	}
	normal := api.BatteryNormal

	for _, tc := range []struct {
		name   string
		reg    dispatchBattery
		regPv  float64
		bb     []dispatchBattery
		demand float64
		res    []float64
	}{
		{
			"equal soc: discharge shared by capacity",
			regulating(55), 0, []dispatchBattery{controlled(55, 0, normal)},
			900,
			[]float64{300.0},
		},
		{
			"equal soc: charge from the self-regulating battery's pv",
			regulating(55), 1000, []dispatchBattery{controlled(55, 0, normal)},
			-600,
			[]float64{-200.0},
		},
		{
			"no demand",
			regulating(55), 0, []dispatchBattery{controlled(55, 0, normal)},
			0,
			[]float64{0.0},
		},
		{
			"controlled fuller: discharges more",
			// scaled soc 0.5 and 0.6, mean 0.533: factors 6.67 and 8.33
			regulating(55), 0, []dispatchBattery{controlled(64, 0, normal)},
			900,
			[]float64{500.0},
		},
		{
			"controlled emptier: no discharge",
			regulating(64), 0, []dispatchBattery{controlled(46, 0, normal)},
			900,
			[]float64{0.0},
		},
		{
			"controlled emptier: charges more",
			// factors 3.33 and 10
			regulating(64), 1500, []dispatchBattery{controlled(46, 0, normal)},
			-1200,
			[]float64{-900.0},
		},
		{
			"self-regulating output limited: controlled takes the rest",
			dispatchBattery{mode: normal, soc: 55, minSoc: 10, maxSoc: 100, capacity: 10, maxCharge: 5000, maxDischarge: 500}, 0,
			[]dispatchBattery{controlled(55, 0, normal)},
			900,
			[]float64{400.0},
		},
		{
			"self-regulating empty: controlled covers the demand",
			regulating(10), 0, []dispatchBattery{controlled(65, 0, normal)},
			900,
			[]float64{800.0}, // max discharge
		},
		{
			"self-regulating full: controlled takes the surplus",
			regulating(100), 1500, []dispatchBattery{controlled(50, 0, normal)},
			-1200,
			[]float64{-1200.0},
		},
		{
			"self-regulating nearly full: controlled charges at max power",
			regulating(95), 3500, []dispatchBattery{controlled(50, 0, normal)},
			-3000,
			[]float64{-1500.0},
		},
		{
			"own pv beyond consumption is stored",
			// the self-regulating battery's share of the surplus would be AC charging
			regulating(55), 0, []dispatchBattery{controlled(55, 1000, normal)},
			-700,
			[]float64{300.0},
		},
		{
			"own pv beyond max discharge is stored",
			regulating(55), 0, []dispatchBattery{controlled(55, 1000, normal)},
			300,
			[]float64{800.0},
		},
		{
			"holddischarge passes own pv through",
			regulating(50), 0, []dispatchBattery{controlled(65, 200, api.BatteryHoldDischarge)},
			700,
			[]float64{200.0},
		},
		{
			"holddischarge allows charge",
			regulating(64), 1500, []dispatchBattery{controlled(46, 0, api.BatteryHoldDischarge)},
			-1200,
			[]float64{-900.0},
		},
		{
			"holdcharge passes own pv through",
			regulating(64), 1600, []dispatchBattery{controlled(46, 400, api.BatteryHoldCharge)},
			-1200,
			[]float64{400.0},
		},
		{
			"unknown mode treated as normal",
			regulating(55), 0, []dispatchBattery{controlled(55, 0, api.BatteryUnknown)},
			900,
			[]float64{300.0},
		},
		{
			"missing capacity: equal weights",
			dispatchBattery{mode: normal, soc: 55, minSoc: 10, maxSoc: 100, maxCharge: 5000, maxDischarge: 5000}, 0, []dispatchBattery{controlled(55, 0, normal)},
			900,
			[]float64{450.0},
		},
		{
			"usable capacity respects different limits",
			// 55% of 10..100 and 60% of 20..100 are both 0.5, usable capacities 9 and 4 kWh
			regulating(55), 0, []dispatchBattery{{controlled: true, mode: normal, soc: 60, minSoc: 20, maxSoc: 100, capacity: 5, maxCharge: 1500, maxDischarge: 800}},
			900,
			[]float64{276.9},
		},
		{
			"at max soc: own pv passed through",
			regulating(90), 0, []dispatchBattery{withMaxSoc(controlled(90, 300, normal), 90)},
			-100,
			[]float64{300.0},
		},
		{
			"above max soc: discharged on top of curtailed pv",
			regulating(95), 0, []dispatchBattery{withMaxSoc(controlled(95, 300, normal), 90)},
			0,
			[]float64{700.0}, // 300 pv + half of max discharge
		},
		{
			"full: discharged at max power even if exported",
			regulating(100), 2000, []dispatchBattery{controlled(100, 0, normal)},
			-900,
			[]float64{800.0},
		},
		{
			"full in holddischarge: own pv passed through only",
			regulating(100), 0, []dispatchBattery{controlled(100, 300, api.BatteryHoldDischarge)},
			0,
			[]float64{300.0},
		},
		{
			"below min power: stopped",
			regulating(55), 0, []dispatchBattery{withMinPower(controlled(55, 0, normal), 100)},
			200,
			[]float64{0.0},
		},
		{
			"charge below min power: stopped",
			regulating(55), 500, []dispatchBattery{withMinPower(controlled(55, 0, normal), 100)},
			-200,
			[]float64{0.0},
		},
		{
			"running: kept down to min power",
			regulating(55), 0, []dispatchBattery{running(withMinPower(controlled(55, 0, normal), 100))},
			360,
			[]float64{120.0},
		},
		{
			"stopped: not started below start power",
			regulating(55), 0, []dispatchBattery{withMinPower(controlled(55, 0, normal), 100)},
			360,
			[]float64{0.0},
		},
		{
			"stopped: started above start power",
			regulating(55), 0, []dispatchBattery{withMinPower(controlled(55, 0, normal), 100)},
			450,
			[]float64{150.0},
		},
		{
			"three batteries: small demand runs one inverter",
			// 92 each would both be below start power
			regulating(50), 0, []dispatchBattery{withMinPower(controlled(65, 0, normal), 100), withMinPower(controlled(65, 0, normal), 100)},
			200,
			[]float64{0.0, 169.2},
		},
		{
			"three batteries: running inverter keeps running",
			regulating(50), 0, []dispatchBattery{running(withMinPower(controlled(65, 0, normal), 100)), withMinPower(controlled(65, 0, normal), 100)},
			150,
			[]float64{126.9, 0.0},
		},
		{
			"three batteries: equal soc shared",
			regulating(55), 0, []dispatchBattery{withMinPower(controlled(55, 0, normal), 100), withMinPower(controlled(55, 0, normal), 100)},
			1200,
			[]float64{300.0, 300.0},
		},
		{
			"three batteries: beyond max power to the others",
			// scaled soc 0.5, 0.6 and 0.4, the fuller controlled battery's share of 1000 exceeds its max discharge
			regulating(55), 0, []dispatchBattery{controlled(64, 0, normal), controlled(46, 0, normal)},
			2000,
			[]float64{800.0, 0.0},
		},
		{
			"three batteries: emptier charges more",
			regulating(55), 2000, []dispatchBattery{controlled(54, 0, normal), controlled(56, 0, normal)},
			-1200,
			[]float64{-333.3, -266.7},
		},
		{
			"three batteries: held battery's share goes to the others",
			regulating(55), 0, []dispatchBattery{controlled(60, 0, api.BatteryHoldDischarge), controlled(55, 0, normal)},
			1200,
			[]float64{0.0, 400.0},
		},
		{
			"unknown limits: 800W assumed",
			regulating(10), 0, []dispatchBattery{{controlled: true, mode: normal, soc: 65, minSoc: 10, maxSoc: 100, capacity: 5}},
			900,
			[]float64{800},
		},
		{
			"no capacity known: equal weights",
			dispatchBattery{mode: normal, soc: 55, minSoc: 10, maxSoc: 100, maxCharge: 5000, maxDischarge: 5000}, 0,
			[]dispatchBattery{{controlled: true, mode: normal, soc: 55, minSoc: 10, maxSoc: 100, maxCharge: 1500, maxDischarge: 800}},
			900,
			[]float64{450},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			all := append(slices.Clone(tc.bb), tc.reg)
			assumeCapacities(all)
			for i := range all {
				all[i].prepare()
			}

			dispatched := append(slices.Clone(all[:len(tc.bb)]), sumRegulating(all[len(tc.bb):], tc.regPv))

			res := dispatchBatteryPower(dispatched, tc.demand)
			for i := range tc.res {
				assert.InDelta(t, tc.res[i], res[i], 1, "battery %d", i)
			}
		})
	}
}

func TestDispatchBatteryPrepare(t *testing.T) {
	battery := func(soc, pv float64, mode api.BatteryMode) dispatchBattery {
		return dispatchBattery{controlled: true, mode: mode, soc: soc, minSoc: 10, maxSoc: 90, capacity: 5, maxCharge: 1500, maxDischarge: 800, pv: pv}
	}

	for _, tc := range []struct {
		name                    string
		b                       dispatchBattery
		scaled, over, under     float64
		minAC, maxAC, preferred float64
		full, limitsAssumed     bool
	}{
		{"normal", battery(50, 200, api.BatteryNormal), 0.5, 0, 0, -1500, 800, 200, false, false},
		{"holddischarge passes pv", battery(50, 200, api.BatteryHoldDischarge), 0.5, 0, 0, -1500, 200, 200, false, false},
		{"holdcharge passes pv", battery(50, 200, api.BatteryHoldCharge), 0.5, 0, 0, 200, 800, 200, true, false},
		{"below min soc passes pv", battery(5, 200, api.BatteryNormal), -0.0625, 0, 0.5, -1500, 200, 200, false, false},
		{"above max soc discharges", battery(95, 300, api.BatteryNormal), 1.0625, 0.5, 0, 700, 800, 700, true, false},
		{"at 100% at max power", battery(100, 300, api.BatteryNormal), 1.125, 1, 0, 800, 800, 800, true, false},
		{"pv beyond max discharge", battery(50, 1000, api.BatteryNormal), 0.5, 0, 0, -1500, 800, 800, false, false},
		{"unknown limits", dispatchBattery{controlled: true, mode: api.BatteryNormal, soc: 50, minSoc: 10, maxSoc: 90, capacity: 5}, 0.5, 0, 0, -800, 800, 0, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.b
			b.prepare()
			assert.InDelta(t, tc.scaled, b.scaledSoc, 1e-9, "scaled")
			assert.InDelta(t, tc.over, b.overcharged, 1e-9, "overcharged")
			assert.InDelta(t, tc.under, b.undercharged, 1e-9, "undercharged")
			assert.Equal(t, tc.minAC, b.minAC, "min")
			assert.Equal(t, tc.maxAC, b.maxAC, "max")
			assert.Equal(t, tc.preferred, b.preferred, "preferred")
			assert.Equal(t, tc.full, b.full, "full")
			assert.Equal(t, tc.limitsAssumed, b.limitsAssumed, "limits assumed")
			assert.InDelta(t, 4.0, b.usableCapacity, 1e-9, "usable capacity")
		})
	}
}

func TestAssumeCapacities(t *testing.T) {
	bb := []dispatchBattery{{}, {capacity: 4}, {capacity: 6}}
	assumeCapacities(bb)
	assert.Equal(t, 5.0, bb[0].capacity)
	assert.True(t, bb[0].capacityAssumed)
	assert.False(t, bb[1].capacityAssumed)

	bb = []dispatchBattery{{}, {}}
	assumeCapacities(bb)
	assert.Equal(t, 1.0, bb[0].capacity)
	assert.Equal(t, 1.0, bb[1].capacity)
}

func TestSumRegulating(t *testing.T) {
	battery := func(soc, capacity, maxDischarge float64, mode api.BatteryMode) dispatchBattery {
		b := dispatchBattery{mode: mode, soc: soc, minSoc: 10, maxSoc: 100, capacity: capacity, maxCharge: 1000, maxDischarge: maxDischarge}
		b.prepare()
		return b
	}

	// weighted by usable capacity, empty batteries add no output
	r := sumRegulating([]dispatchBattery{
		battery(55, 10, 3000, api.BatteryNormal),
		battery(10, 5, 2000, api.BatteryNormal),
	}, 500)
	assert.InDelta(t, 13.5, r.usableCapacity, 1e-9)
	assert.InDelta(t, 1.0/3, r.scaledSoc, 1e-9)
	assert.Equal(t, 0.0, r.minAC)
	assert.Equal(t, 3500.0, r.maxAC)
	assert.Equal(t, 500.0, r.preferred)

	// none can charge: pv passed through
	r = sumRegulating([]dispatchBattery{
		battery(100, 10, 2000, api.BatteryNormal),
		battery(50, 10, 2000, api.BatteryHoldCharge),
	}, 300)
	assert.True(t, r.full)
	assert.Equal(t, 300.0, r.minAC)
	assert.Equal(t, 4300.0, r.maxAC)
}

func TestDispatchTable(t *testing.T) {
	bb := []dispatchBattery{
		{name: "controlled", controlled: true, mode: api.BatteryNormal, soc: 50, minSoc: 10, maxSoc: 90, pv: 200},
		{name: "regulating", mode: api.BatteryNormal, soc: 60, minSoc: 10, maxSoc: 100, capacity: 10, maxCharge: 5000, maxDischarge: 5000},
	}
	assumeCapacities(bb)
	for i := range bb {
		bb[i].prepare()
	}
	bb = append(bb, sumRegulating(bb[1:], 1000))

	table := dispatchTable(bb)
	assert.Contains(t, table, "8.0*")  // assumed capacity, usable kWh
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
