package core

import (
	"testing"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/stretchr/testify/assert"
)

func TestDispatchBatteryPower(t *testing.T) {
	regulating := func(soc float64) dispatchBattery {
		return dispatchBattery{mode: api.BatteryNormal, soc: new(soc), minSoc: 10, maxSoc: 100, capacity: 10}
	}
	controlled := func(soc float64, mode api.BatteryMode) dispatchBattery {
		return dispatchBattery{controlled: true, mode: mode, soc: new(soc), minSoc: 10, maxSoc: 100, capacity: 5, maxCharge: 1500, maxDischarge: 800}
	}
	withMode := func(b dispatchBattery, mode api.BatteryMode) dispatchBattery {
		b.mode = mode
		return b
	}
	withMinPower := func(b dispatchBattery, p float64) dispatchBattery {
		b.minPower = p
		return b
	}
	withSetpoint := func(b dispatchBattery, p float64) dispatchBattery {
		b.setpoint = p
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
			"equal soc discharge: share by capacity",
			[]dispatchBattery{regulating(55), controlled(55, normal)},
			900,
			[]*float64{nil, new(300.0)},
		},
		{
			"controlled fuller: discharges double share",
			[]dispatchBattery{regulating(50), controlled(65, normal)},
			900,
			[]*float64{nil, new(600.0)},
		},
		{
			"controlled emptier: no discharge",
			[]dispatchBattery{regulating(60), controlled(45, normal)},
			900,
			[]*float64{nil, new(0.0)},
		},
		{
			"self-regulating empty: controlled covers the import",
			[]dispatchBattery{regulating(10), controlled(65, normal)},
			900,
			[]*float64{nil, new(800.0)}, // limited by max discharge
		},
		{
			"controlled emptier: charges double share",
			[]dispatchBattery{regulating(60), controlled(45, normal)},
			-1200,
			[]*float64{nil, new(-800.0)},
		},
		{
			"controlled fuller: no charge",
			[]dispatchBattery{regulating(50), controlled(65, normal)},
			-1200,
			[]*float64{nil, new(0.0)},
		},
		{
			"self-regulating full: controlled takes the export",
			[]dispatchBattery{regulating(100), controlled(50, normal)},
			-1200,
			[]*float64{nil, new(-1200.0)}, // capped at the surplus
		},
		{
			"charge limited",
			[]dispatchBattery{regulating(60), controlled(45, normal)},
			-3000,
			[]*float64{nil, new(-1500.0)},
		},
		{
			"no demand",
			[]dispatchBattery{regulating(55), controlled(55, normal)},
			0,
			[]*float64{nil, new(0.0)},
		},
		{
			"holddischarge blocks discharge",
			[]dispatchBattery{regulating(50), controlled(65, api.BatteryHoldDischarge)},
			900,
			[]*float64{nil, new(0.0)},
		},
		{
			"holddischarge allows charge",
			[]dispatchBattery{regulating(60), controlled(45, api.BatteryHoldDischarge)},
			-1200,
			[]*float64{nil, new(-800.0)},
		},
		{
			"holdcharge blocks charge",
			[]dispatchBattery{regulating(60), controlled(45, api.BatteryHoldCharge)},
			-1200,
			[]*float64{nil, new(0.0)},
		},
		{
			"held self-regulating: controlled takes its share",
			[]dispatchBattery{withMode(regulating(55), api.BatteryHoldDischarge), controlled(55, normal)},
			900,
			[]*float64{nil, new(800.0)}, // limited by max discharge
		},
		{
			"unknown mode treated as normal",
			[]dispatchBattery{regulating(55), controlled(55, api.BatteryUnknown)},
			900,
			[]*float64{nil, new(300.0)},
		},
		{
			"forced charge owns the device",
			[]dispatchBattery{regulating(55), controlled(55, api.BatteryCharge)},
			900,
			[]*float64{nil, nil},
		},
		{
			"forced discharge owns the device",
			[]dispatchBattery{regulating(55), controlled(55, api.BatteryDischarge)},
			900,
			[]*float64{nil, nil},
		},
		{
			"at min soc: no discharge",
			[]dispatchBattery{regulating(10), controlled(10, normal)},
			900,
			[]*float64{nil, new(0.0)},
		},
		{
			"at max soc: no charge, discharged so pv is not curtailed",
			[]dispatchBattery{regulating(100), controlled(100, normal)},
			-900,
			[]*float64{nil, new(800.0)},
		},
		{
			"scaled soc respects different limits",
			// self-regulating 55% of 10..100 = 0.5, controlled 60% of 20..100 = 0.5
			[]dispatchBattery{regulating(55), {controlled: true, mode: normal, soc: new(60.0), minSoc: 20, maxSoc: 100, capacity: 5}},
			900,
			[]*float64{nil, new(300.0)},
		},
		{
			"missing capacity: equal weights",
			[]dispatchBattery{{mode: normal, soc: new(55.0), minSoc: 10, maxSoc: 100}, controlled(55, normal)},
			900,
			[]*float64{nil, new(450.0)},
		},
		{
			"missing capacity: still balanced against the self-regulating battery",
			[]dispatchBattery{{mode: normal, soc: new(50.0), minSoc: 10, maxSoc: 100}, controlled(65, normal)},
			900,
			[]*float64{nil, new(800.0)}, // 825 limited by max discharge
		},
		{
			"missing soc: not dispatched",
			[]dispatchBattery{regulating(55), {controlled: true, mode: normal, capacity: 5}},
			900,
			[]*float64{nil, nil},
		},
		{
			"below min power: stopped",
			[]dispatchBattery{regulating(55), withMinPower(controlled(55, normal), 100)},
			200,
			[]*float64{nil, new(0.0)},
		},
		{
			"charge below min power: stopped",
			[]dispatchBattery{regulating(55), withMinPower(controlled(55, normal), 100)},
			-200,
			[]*float64{nil, new(0.0)},
		},
		{
			"three batteries: small demand runs one inverter",
			// 69 each would both be below min power
			[]dispatchBattery{regulating(50), withMinPower(controlled(65, normal), 100), withMinPower(controlled(65, normal), 100)},
			150,
			[]*float64{nil, new(137.5), new(0.0)},
		},
		{
			"three batteries: equal soc consolidated into one",
			[]dispatchBattery{regulating(55), withMinPower(controlled(55, normal), 100), withMinPower(controlled(55, normal), 100)},
			1200,
			[]*float64{nil, new(600.0), new(0.0)},
		},
		{
			"three batteries: overflow beyond max power to the next",
			// scaled soc 0.5, 0.6 and 0.4, the fuller controlled battery's share of 1000 exceeds its max discharge
			[]dispatchBattery{regulating(55), controlled(64, normal), controlled(46, normal)},
			2000,
			[]*float64{nil, new(800.0), new(200.0)},
		},
		{
			"three batteries: fullest discharges",
			[]dispatchBattery{regulating(55), controlled(54, normal), controlled(56, normal)},
			1200,
			[]*float64{nil, new(0.0), new(600.0)},
		},
		{
			"three batteries: emptiest charges",
			[]dispatchBattery{regulating(55), controlled(54, normal), controlled(56, normal)},
			-1200,
			[]*float64{nil, new(-600.0), new(0.0)},
		},
		{
			"three batteries: running battery kept within switch spread",
			[]dispatchBattery{regulating(55), withSetpoint(controlled(54, normal), 600), controlled(57, normal)},
			1200,
			[]*float64{nil, new(617.0), new(0.0)}, // shares 258 + 358
		},
		{
			"three batteries: switched over beyond switch spread",
			[]dispatchBattery{regulating(55), withSetpoint(controlled(52, normal), 600), controlled(58, normal)},
			1200,
			[]*float64{nil, new(0.0), new(600.0)},
		},
		{
			"three batteries: running charge does not hold discharge",
			[]dispatchBattery{regulating(55), withSetpoint(controlled(54, normal), -600), controlled(56, normal)},
			1200,
			[]*float64{nil, new(0.0), new(600.0)},
		},
		{
			"three batteries: held battery's share goes to the others",
			[]dispatchBattery{regulating(55), controlled(60, api.BatteryHoldDischarge), controlled(55, normal)},
			1200,
			[]*float64{nil, new(0.0), new(344.0)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := dispatchBatteryPower(tc.bb, tc.demand, 5000)
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

func TestDispatchBatteryACPower(t *testing.T) {
	regulating := func(soc float64) dispatchBattery {
		return dispatchBattery{mode: api.BatteryNormal, soc: new(soc), minSoc: 10, maxSoc: 100, capacity: 10}
	}
	controlled := func(soc, pv float64, mode api.BatteryMode) dispatchBattery {
		return dispatchBattery{controlled: true, mode: mode, soc: new(soc), minSoc: 10, maxSoc: 100, capacity: 5, maxCharge: 1500, maxDischarge: 800, minPower: 100, pv: pv}
	}
	withMaxSoc := func(b dispatchBattery, soc float64) dispatchBattery {
		b.maxSoc = soc
		return b
	}
	normal := api.BatteryNormal

	for _, tc := range []struct {
		name                string
		bb                  []dispatchBattery
		demand, consumption float64
		ac                  float64
	}{
		{
			"discharge adds own pv",
			[]dispatchBattery{regulating(50), controlled(65, 200, normal)},
			900, 1100,
			800, // 600 discharge + 200 pv
		},
		{
			"discharge limited on the AC side",
			[]dispatchBattery{regulating(50), controlled(65, 300, normal)},
			900, 1200,
			800, // 500 discharge + 300 pv
		},
		{
			"own pv beyond consumption is stored",
			// share 233 charge would feed 767, the self-regulating battery cannot store the export
			[]dispatchBattery{regulating(55), controlled(55, 1000, normal)},
			-700, 300,
			300,
		},
		{
			"nearly full below max soc: own pv beyond consumption stored",
			[]dispatchBattery{regulating(99), controlled(99, 700, normal)},
			-400, 300,
			300,
		},
		{
			"at max soc: own pv passed through",
			[]dispatchBattery{regulating(90), withMaxSoc(controlled(90, 300, normal), 90)},
			0, 200,
			300,
		},
		{
			"above max soc: discharged above curtailed pv",
			[]dispatchBattery{regulating(95), withMaxSoc(controlled(95, 300, normal), 90)},
			0, 300,
			400, // half of max discharge
		},
		{
			"full: discharged at max power",
			[]dispatchBattery{regulating(100), controlled(100, 300, normal)},
			0, 300,
			800,
		},
		{
			"full in holddischarge: own pv passed through only",
			[]dispatchBattery{regulating(100), controlled(100, 300, api.BatteryHoldDischarge)},
			0, 200,
			300,
		},
		{
			"full while discharging: max power",
			[]dispatchBattery{regulating(100), controlled(100, 200, normal)},
			300, 500,
			800,
		},
		{
			"holddischarge passes own pv through",
			[]dispatchBattery{regulating(50), controlled(65, 200, api.BatteryHoldDischarge)},
			700, 900,
			200,
		},
		{
			"holddischarge stores own pv beyond consumption",
			[]dispatchBattery{regulating(50), controlled(65, 600, api.BatteryHoldDischarge)},
			0, 400,
			400,
		},
		{
			"self-regulating nearly full: controlled charges from the grid side",
			[]dispatchBattery{regulating(95), controlled(50, 0, normal)},
			-3000, 500,
			-1500,
		},
		{
			"charge offset by own pv",
			[]dispatchBattery{regulating(60), controlled(45, 400, normal)},
			-1200, 300,
			-400, // 800 charge - 400 pv
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := dispatchBatteryPower(tc.bb, tc.demand, tc.consumption)
			if assert.NotNil(t, res[1]) {
				assert.InDelta(t, tc.ac, *res[1], 1)
			}
		})
	}
}

func TestDispatchDemand(t *testing.T) {
	record := func(vals ...float64) *Site {
		site := &Site{}
		now := time.Now()
		for i, p := range vals {
			site.recordDemand(now.Add(time.Duration(i)*30*time.Second), p, 1000+p)
		}
		return site
	}

	demand, consumption := (&Site{}).dispatchDemand()
	assert.Zero(t, demand)
	assert.Zero(t, consumption)

	// samples older than the window are dropped
	site := record(900, 300, 2500, 400, 350, 800, 600, 500, 700, 450, 3000, 650)
	assert.Len(t, site.demandWindow, 11)

	// deficit: low percentile, short load peaks do not raise it
	demand, consumption = site.dispatchDemand()
	assert.Equal(t, 350.0, demand)
	assert.Equal(t, 1350.0, consumption)

	// surplus: high percentile, short pv peaks do not raise it
	site = record(-800, -1200, -300, -900, -1000, -700, -1100, -600, -950, -850, -750)
	demand, consumption = site.dispatchDemand()
	assert.Equal(t, -600.0, demand)
	assert.Equal(t, 0.0, consumption) // low percentile -1100 + 1000, not negative

	// fluctuating around zero
	site = record(-800, -500, -300, 200, 400, 600, -100, 300, -200, 500, 100)
	demand, _ = site.dispatchDemand()
	assert.Zero(t, demand)
}
