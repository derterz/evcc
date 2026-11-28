package core

import (
	"errors"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/types"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testLog = util.NewLogger("test")

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

	// the uncontrolled battery charges from its own pv only
	uncontrolled := func(soc, pv float64) dispatchBattery {
		return newDispatchBattery(testLog, measurement(soc, 10, 100, 10), normal, 10, 0, 5000, pv)
	}
	controlled := func(soc, pv float64, mode api.BatteryMode) dispatchBattery {
		return newDispatchBattery(testLog, measurement(soc, 10, 100, 5), mode, 5, 1500, 800, pv)
	}
	controlledMaxSoc90 := func(soc, pv float64) dispatchBattery {
		return newDispatchBattery(testLog, measurement(soc, 10, 90, 5), normal, 5, 1500, 800, pv)
	}
	withMinPower := func(b dispatchBattery, p float64) dispatchBattery {
		b.minPower = p
		return b
	}

	for _, tc := range []struct {
		name  string
		reg   dispatchBattery
		bb    []dispatchBattery
		total float64
		res   []float64
	}{
		{"equal soc: discharge shared by capacity", uncontrolled(55, 0), []dispatchBattery{controlled(55, 0, normal)}, 900, []float64{300}},
		{"equal soc: charge from the uncontrolled battery's pv", uncontrolled(55, 1000), []dispatchBattery{controlled(55, 0, normal)}, 400, []float64{-200}},
		{"no consumption", uncontrolled(55, 0), []dispatchBattery{controlled(55, 0, normal)}, 0, []float64{0}},
		// scaled soc 0.5 and 0.6, mean 0.533: factors 6.67 and 8.33
		{"controlled fuller: discharges more", uncontrolled(55, 0), []dispatchBattery{controlled(64, 0, normal)}, 900, []float64{500}},
		{"controlled emptier: no discharge", uncontrolled(64, 0), []dispatchBattery{controlled(46, 0, normal)}, 900, []float64{0}},
		// factors 3.33 and 10
		{"controlled emptier: charges more", uncontrolled(64, 1500), []dispatchBattery{controlled(46, 0, normal)}, 300, []float64{-900}},
		{
			"uncontrolled output limited: controlled takes the rest",
			newDispatchBattery(testLog, measurement(55, 10, 100, 10), normal, 10, 0, 500, 0),
			[]dispatchBattery{controlled(55, 0, normal)}, 900, []float64{400},
		},
		{"uncontrolled empty: controlled covers the consumption", uncontrolled(10, 0), []dispatchBattery{controlled(65, 0, normal)}, 900, []float64{800}},
		{"uncontrolled at max soc: controlled takes the surplus", uncontrolled(99, 1500), []dispatchBattery{controlled(50, 0, normal)}, 300, []float64{-1200}},
		// at 100% the uncontrolled battery is expected to discharge at max power
		{"uncontrolled at 100%: controlled charges at max power", uncontrolled(100, 1500), []dispatchBattery{controlled(50, 0, normal)}, 300, []float64{-1500}},
		{
			"uncontrolled in holdcharge: controlled takes the surplus",
			newDispatchBattery(testLog, measurement(50, 10, 100, 10), api.BatteryHoldCharge, 10, 0, 7000, 2000),
			[]dispatchBattery{controlled(50, 0, normal)}, 500, []float64{-1500},
		},
		{"uncontrolled nearly full: controlled charges at max power", uncontrolled(95, 3500), []dispatchBattery{controlled(50, 0, normal)}, 500, []float64{-1500}},
		// the uncontrolled battery's share of the surplus would be AC charging
		{"own pv beyond consumption is stored", uncontrolled(55, 0), []dispatchBattery{controlled(55, 1000, normal)}, 300, []float64{300}},
		{"own pv beyond max discharge is stored", uncontrolled(55, 0), []dispatchBattery{controlled(55, 1000, normal)}, 1300, []float64{800}},
		{"holddischarge passes own pv through", uncontrolled(50, 0), []dispatchBattery{controlled(65, 200, api.BatteryHoldDischarge)}, 900, []float64{200}},
		{"holddischarge allows charge", uncontrolled(64, 1500), []dispatchBattery{controlled(46, 0, api.BatteryHoldDischarge)}, 300, []float64{-900}},
		{"holdcharge passes own pv through", uncontrolled(64, 1600), []dispatchBattery{controlled(46, 400, api.BatteryHoldCharge)}, 800, []float64{400}},
		{"unknown mode treated as normal", uncontrolled(55, 0), []dispatchBattery{controlled(55, 0, api.BatteryUnknown)}, 900, []float64{300}},
		{
			"assumed capacity: equal weights",
			newDispatchBattery(testLog, measurement(55, 10, 100, 0), normal, 5, 0, 5000, 0),
			[]dispatchBattery{controlled(55, 0, normal)}, 900, []float64{450},
		},
		{
			// 55% of 10..100 and 60% of 20..100 are both 0.5, usable capacities 9 and 4 kWh
			"usable capacity respects different limits",
			uncontrolled(55, 0),
			[]dispatchBattery{newDispatchBattery(testLog, measurement(60, 20, 100, 5), normal, 5, 1500, 800, 0)}, 900, []float64{276.9},
		},
		{"at max soc: own pv passed through", uncontrolled(90, 0), []dispatchBattery{controlledMaxSoc90(90, 300)}, 200, []float64{300}},
		// 300 pv + half of max discharge
		{"above max soc: discharged on top of curtailed pv", uncontrolled(95, 0), []dispatchBattery{controlledMaxSoc90(95, 300)}, 300, []float64{700}},
		{"full: discharged at max power even if exported", uncontrolled(100, 2000), []dispatchBattery{controlled(100, 0, normal)}, 1100, []float64{800}},
		{"full in holddischarge: discharged at max power", uncontrolled(100, 0), []dispatchBattery{controlled(100, 300, api.BatteryHoldDischarge)}, 300, []float64{800}},
		{"below min power: stopped", uncontrolled(55, 0), []dispatchBattery{withMinPower(controlled(55, 0, normal), 100)}, 200, []float64{0}},
		{"charge below min power: stopped", uncontrolled(55, 500), []dispatchBattery{withMinPower(controlled(55, 0, normal), 100)}, 300, []float64{0}},
		{"above min power: runs", uncontrolled(55, 0), []dispatchBattery{withMinPower(controlled(55, 0, normal), 100)}, 360, []float64{120}},
		{
			// 92 each would both be below min power
			"three batteries: small consumption runs one inverter",
			uncontrolled(50, 0),
			[]dispatchBattery{withMinPower(controlled(65, 0, normal), 100), withMinPower(controlled(65, 0, normal), 100)}, 200, []float64{0, 169.2},
		},
		{
			"three batteries: equal soc shared",
			uncontrolled(55, 0),
			[]dispatchBattery{withMinPower(controlled(55, 0, normal), 100), withMinPower(controlled(55, 0, normal), 100)}, 1200, []float64{300, 300},
		},
		{
			// scaled soc 0.5, 0.6 and 0.4, the fuller controlled battery's share of 1000 exceeds its max discharge
			"three batteries: beyond max power to the others",
			uncontrolled(55, 0),
			[]dispatchBattery{controlled(64, 0, normal), controlled(46, 0, normal)}, 2000, []float64{800, 0},
		},
		{
			"three batteries: emptier charges more",
			uncontrolled(55, 2000),
			[]dispatchBattery{controlled(54, 0, normal), controlled(56, 0, normal)}, 800, []float64{-333.3, -266.7},
		},
		{
			// the pinned battery charges 1000W from the grid, without soc it is not balanced
			"pinned battery keeps its power",
			uncontrolled(55, 0),
			[]dispatchBattery{controlled(55, 0, normal), pinnedDispatchBattery(testLog, types.Measurement{}, api.BatteryCharge, 5, 0, -1000)}, 900, []float64{633.3, -1000},
		},
		{
			"three batteries: held battery's share goes to the others",
			uncontrolled(55, 0),
			[]dispatchBattery{controlled(60, 0, api.BatteryHoldDischarge), controlled(55, 0, normal)}, 1200, []float64{0, 400},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dispatched := append(slices.Clone(tc.bb), tc.reg)

			res := dispatchBatteryPower(dispatched, tc.total)
			for i := range tc.res {
				assert.InDelta(t, tc.res[i], res[i], 1, "battery %d", i)
			}
		})
	}
}

func TestNewDispatchBattery(t *testing.T) {
	controlled := func(soc, pv float64, mode api.BatteryMode) dispatchBattery {
		return newDispatchBattery(testLog, measurement(soc, 10, 90, 5), mode, 5, 1500, 800, pv)
	}

	for _, tc := range []struct {
		name                    string
		b                       dispatchBattery
		scaled                  float64
		minAC, maxAC, preferred float64
		targetCharge            float64
	}{
		{"normal", controlled(50, 200, api.BatteryNormal), 0.5, -1500, 800, 200, 0},
		{"holddischarge passes pv", controlled(50, 200, api.BatteryHoldDischarge), 0.5, -1500, 200, 200, 0},
		{"holdcharge passes pv", controlled(50, 200, api.BatteryHoldCharge), 0.5, 200, 800, 200, 0},
		{"below min soc charges", controlled(5, 200, api.BatteryNormal), -0.0625, -1500, -550, -550, 750},
		{"at 0% at max charge", controlled(0, 200, api.BatteryNormal), -0.125, -1500, -1300, -1300, 1500},
		{"below min soc in holdcharge charges", controlled(5, 200, api.BatteryHoldCharge), -0.0625, -550, -550, -550, 750},
		{"above max soc in holddischarge discharges", controlled(95, 300, api.BatteryHoldDischarge), 1.0625, 700, 700, 700, -400},
		{"above max soc discharges", controlled(95, 300, api.BatteryNormal), 1.0625, 700, 800, 700, -400},
		{"at 100% at max power", controlled(100, 300, api.BatteryNormal), 1.125, 800, 800, 800, -500},
		{"pv beyond max discharge", controlled(50, 1000, api.BatteryNormal), 0.5, -1500, 800, 800, 0},
		{"charges from own pv only", newDispatchBattery(testLog, measurement(50, 10, 90, 5), api.BatteryNormal, 5, 0, 800, 300), 0.5, 0, 800, 300, 0},
		{"charges from own pv only, full", newDispatchBattery(testLog, measurement(95, 10, 90, 5), api.BatteryNormal, 5, 0, 800, 300), 1.0625, 700, 800, 700, -400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.b
			assert.InDelta(t, tc.scaled, b.scaledSoc, 1e-9, "scaled")
			assert.Equal(t, tc.minAC, b.minAC, "min")
			assert.Equal(t, tc.maxAC, b.maxAC, "max")
			assert.Equal(t, tc.preferred, b.preferred(), "preferred")
			assert.Equal(t, tc.targetCharge, b.targetCharge, "target charge")
			assert.InDelta(t, 4.0, b.usableCapacity, 1e-9, "usable capacity")
		})
	}

	// unknown soc limits
	b := newDispatchBattery(testLog, types.Measurement{Soc: new(50.0)}, api.BatteryNormal, 3, 1500, 800, 0)
	assert.Equal(t, 3.0, b.usableCapacity)
	assert.Equal(t, 0.5, b.scaledSoc)
}

func TestMeasuredCapacity(t *testing.T) {
	for _, c := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		_, ok := measuredCapacity(measurement(50, 0, 100, c))
		assert.False(t, ok, "capacity %v", c)
	}

	c, ok := measuredCapacity(measurement(50, 0, 100, 4))
	assert.True(t, ok)
	assert.Equal(t, 4.0, c)
}

func TestNewDispatchBatteryInvalidSocLimits(t *testing.T) {
	m := types.Measurement{Soc: new(50.0), MinSoc: new(math.NaN()), MaxSoc: new(90.0)}
	b := newDispatchBattery(testLog, m, api.BatteryNormal, 3, 1500, 800, 0)
	assert.Equal(t, 0.5, b.scaledSoc)
	assert.Equal(t, 3.0, b.usableCapacity)
}

func TestDispatchBatteryString(t *testing.T) {
	b := newDispatchBattery(testLog, measurement(95, 10, 90, 5), api.BatteryNormal, 5, 1500, 800, 300)
	b.name = "battery"
	assert.Equal(t, "battery (normal): soc 106%, capacity 4.0kWh, pv 300W, target charge -400W, ac 700..800W", b.String())
}

func TestConsumptionPercentile(t *testing.T) {
	record := func(vals ...float64) *Site {
		site := &Site{}
		now := time.Now()
		for i, p := range vals {
			site.recordConsumption(now.Add(time.Duration(i)*time.Minute), p)
		}
		return site
	}

	low, high := (&Site{}).consumptionPercentiles()
	assert.Zero(t, low)
	assert.Zero(t, high)

	// invalid samples are not recorded
	site := &Site{}
	site.recordConsumption(time.Now(), math.NaN())
	site.recordConsumption(time.Now(), math.Inf(-1))
	site.recordConsumption(time.Now(), -100)
	assert.Empty(t, site.consumptionWindow)

	// samples older than the window are dropped
	site = record(900, 300, 2500, 400, 350, 800, 600, 500, 700, 450, 3000, 650)
	assert.Len(t, site.consumptionWindow, 11)

	low, high = site.consumptionPercentiles()
	assert.Equal(t, 350.0, low)
	assert.Equal(t, 2500.0, high)

	site = record(0, 0, 0, 200, 300)
	low, high = site.consumptionPercentiles()
	assert.Zero(t, low)
	assert.Equal(t, 300.0, high)
}

func TestDispatchTotal(t *testing.T) {
	// preferred 200 and 800W own pv
	bb := []dispatchBattery{
		newDispatchBattery(testLog, measurement(55, 10, 100, 5), api.BatteryNormal, 5, 1500, 800, 200),
		newDispatchBattery(testLog, measurement(55, 10, 100, 10), api.BatteryNormal, 10, 0, 5800, 800),
	}

	assert.Equal(t, 1500.0, dispatchTotal(bb, 1500, 2500), "deficit: low consumption")
	assert.Equal(t, 600.0, dispatchTotal(bb, 300, 600), "surplus: high consumption")
	assert.Equal(t, 1000.0, dispatchTotal(bb, 300, 1800), "fluctuating: preferred")

	// below min soc charging 750W from the grid, preferred -550W
	bb[0] = newDispatchBattery(testLog, measurement(5, 10, 100, 5), api.BatteryNormal, 5, 1500, 800, 200)
	assert.Equal(t, 750.0, dispatchTotal(bb, 1500, 2500), "target charge not covered by the others")
	assert.Equal(t, -150.0, dispatchTotal(bb, 300, 600), "target charge with surplus")
}

type dispatchMeter struct {
	api.Meter
	pv, minPower float64
	set          []float64
	err          error
}

func (m *dispatchMeter) SetBatteryPower(power float64) error {
	m.set = append(m.set, power)
	return m.err
}

func (m *dispatchMeter) BatteryPvPower() (float64, error) {
	return m.pv, nil
}

func (m *dispatchMeter) MinBatteryPower() float64 {
	return m.minPower
}

func (m *dispatchMeter) GetPowerLimits() (float64, float64, error) {
	return 1500, 800, nil
}

// pick returns the elements at the given indices
func pick[T any](s []T, idx []int) []T {
	res := make([]T, 0, len(idx))
	for _, i := range idx {
		res = append(res, s[i])
	}
	return res
}

func TestDispatchBatteries(t *testing.T) {
	setup := func(consumption float64) (*Site, *dispatchMeter, *dispatchMeter, *dispatchMeter) {
		ctrl := &dispatchMeter{Meter: &mockMeter{}, pv: 200}
		noSoc := &dispatchMeter{Meter: &mockMeter{}}
		forced := &dispatchMeter{Meter: &mockMeter{}}

		site := &Site{
			log:                util.NewLogger("test"),
			batteryMode:        api.BatteryNormal,
			batteryModeApplied: map[string]api.BatteryMode{"forced": api.BatteryCharge},
		}
		site.batteryMeters = []config.Device[api.Meter]{
			config.NewStaticDevice[api.Meter](config.Named{Name: "ctrl"}, ctrl),
			config.NewStaticDevice[api.Meter](config.Named{Name: "uncontrolled"}, &mockBatteryPowerLimiter{Meter: &mockMeter{}, charge: 5000, discharge: 5000}),
			config.NewStaticDevice[api.Meter](config.Named{Name: "unknown"}, &mockMeter{}),
			config.NewStaticDevice[api.Meter](config.Named{Name: "nosoc"}, noSoc),
			config.NewStaticDevice[api.Meter](config.Named{Name: "forced"}, forced),
		}
		site.pvPower = 1000
		site.battery.Devices = []types.Measurement{
			measurement(55, 10, 100, 5),
			measurement(55, 10, 100, 10),
			measurement(55, 10, 100, 0),
			{},
			measurement(55, 10, 100, 5),
		}
		site.recordConsumption(time.Now(), consumption)

		return site, ctrl, noSoc, forced
	}

	t.Run("unknown capacity is the average", func(t *testing.T) {
		// usable capacities 4.5, 9 and 6 for the average of 5, 10 and 5 kWh, consumption beyond pv is split by them
		site, ctrl, noSoc, forced := setup(2000)
		site.dispatchBatteries()

		if assert.Len(t, ctrl.set, 1) {
			assert.InDelta(t, 200+1000*4.5/19.5, ctrl.set[0], 1e-6)
		}
		assert.Empty(t, noSoc.set, "no soc")
		assert.Empty(t, forced.set, "forced mode")
		assert.True(t, site.batteryDispatched["ctrl"])
	})

	t.Run("no consumption: pv surplus split by capacity", func(t *testing.T) {
		site, ctrl, _, _ := setup(0)
		site.dispatchBatteries()

		if assert.Len(t, ctrl.set, 1) {
			assert.InDelta(t, 200-1000*4.5/19.5, ctrl.set[0], 1e-6)
		}
	})

	t.Run("no uncontrolled battery: other pv covers consumption", func(t *testing.T) {
		// 800W pv not from the controlled battery leaves 300W surplus
		site, ctrl, _, _ := setup(500)
		keep := []int{0, 3, 4}
		site.batteryMeters = pick(site.batteryMeters, keep)
		site.battery.Devices = pick(site.battery.Devices, keep)
		site.dispatchBatteries()

		if assert.Len(t, ctrl.set, 1) {
			assert.InDelta(t, -300, ctrl.set[0], 1e-6)
		}
	})

	t.Run("forced battery's power taken as given", func(t *testing.T) {
		// the forced battery discharges 500W, the others share the remaining 500W beyond their pv
		site, ctrl, _, forced := setup(2000)
		site.battery.Devices[4].Power = 500
		site.dispatchBatteries()

		if assert.Len(t, ctrl.set, 1) {
			assert.InDelta(t, 200+500*4.5/19.5, ctrl.set[0], 1e-6)
		}
		assert.Empty(t, forced.set)
	})

	t.Run("forced uncontrolled battery's pv not counted as external", func(t *testing.T) {
		// both uncontrolled batteries store their 800W pv, so the controlled one covers the consumption
		site, ctrl, _, _ := setup(500)
		site.batteryModeApplied["uncontrolled"] = api.BatteryCharge
		site.batteryModeApplied["unknown"] = api.BatteryCharge
		site.battery.Devices[1].Power = -480
		site.battery.Devices[2].Power = -320
		site.dispatchBatteries()

		if assert.Len(t, ctrl.set, 1) {
			assert.InDelta(t, 500, ctrl.set[0], 1e-6)
		}
	})

	t.Run("battery without soc passes its pv", func(t *testing.T) {
		// its measured 300W pv is pinned and not shared again with the uncontrolled batteries, 700W remain beyond pv
		site, ctrl, noSoc, _ := setup(2000)
		noSoc.pv = 300
		site.pvPower = 1300
		site.dispatchBatteries()

		if assert.Len(t, ctrl.set, 1) {
			assert.InDelta(t, 200+700*4.5/19.5, ctrl.set[0], 1e-6)
		}
		assert.Empty(t, noSoc.set)
	})

	t.Run("fluctuating consumption: own pv only", func(t *testing.T) {
		site, ctrl, _, _ := setup(300)
		site.recordConsumption(time.Now(), 1800)
		site.dispatchBatteries()

		if assert.Len(t, ctrl.set, 1) {
			assert.InDelta(t, 200, ctrl.set[0], 1e-6)
		}
	})

	t.Run("nothing controlled", func(t *testing.T) {
		site, ctrl, _, _ := setup(1000)
		site.batteryModeApplied["ctrl"] = api.BatteryDischarge
		site.dispatchBatteries()

		assert.Empty(t, ctrl.set)
		assert.Empty(t, site.batteryDispatched)
	})

	t.Run("stopped once when leaving the dispatch", func(t *testing.T) {
		site, ctrl, _, _ := setup(2000)
		site.dispatchBatteries()
		require.Len(t, ctrl.set, 1)

		site.battery.Devices[0].Soc = nil
		site.dispatchBatteries()
		site.dispatchBatteries()

		assert.Equal(t, []float64{0}, ctrl.set[1:])
		assert.Empty(t, site.batteryDispatched)
	})

	t.Run("not stopped when a forced mode takes over", func(t *testing.T) {
		site, ctrl, _, _ := setup(2000)
		site.dispatchBatteries()

		site.batteryModeApplied["ctrl"] = api.BatteryCharge
		site.dispatchBatteries()

		assert.Len(t, ctrl.set, 1)
		assert.Empty(t, site.batteryDispatched)
	})

	t.Run("stopped without recent consumption", func(t *testing.T) {
		site, ctrl, _, _ := setup(2000)
		site.dispatchBatteries()

		site.consumptionWindow[0].ts = time.Now().Add(-dispatchWindow - time.Second)
		site.dispatchBatteries()

		assert.Equal(t, []float64{0}, ctrl.set[1:])
		assert.Empty(t, site.consumptionWindow)
	})

	t.Run("retried if stopping fails", func(t *testing.T) {
		site, ctrl, _, _ := setup(2000)
		site.dispatchBatteries()

		ctrl.err = errors.New("offline")
		site.battery.Devices[0].Soc = nil
		site.dispatchBatteries()
		assert.True(t, site.batteryDispatched["ctrl"])

		ctrl.err = nil
		site.dispatchBatteries()
		assert.Empty(t, site.batteryDispatched)
	})
}

func TestDispatchBatteryPowerProperties(t *testing.T) {
	rnd := rand.New(rand.NewPCG(1, 2))
	between := func(lo, hi float64) float64 { return lo + rnd.Float64()*(hi-lo) }
	modes := []api.BatteryMode{api.BatteryNormal, api.BatteryHoldCharge, api.BatteryHoldDischarge}

	for range 10000 {
		bb := make([]dispatchBattery, 1+rnd.IntN(4))
		for i := range bb {
			var maxCharge float64
			if rnd.IntN(3) > 0 {
				maxCharge = between(100, 3000)
			}

			capacity := between(1, 15)
			m := measurement(between(0, 100), between(0, 30), between(70, 100), capacity)
			bb[i] = newDispatchBattery(testLog, m, modes[rnd.IntN(len(modes))], capacity, maxCharge, between(100, 3000), between(0, 2000))

			switch rnd.IntN(6) {
			case 0:
				bb[i] = pinnedDispatchBattery(testLog, m, api.BatteryCharge, capacity, between(0, 2000), between(-3000, 3000))
			case 1:
				bb[i] = pinnedDispatchBattery(testLog, types.Measurement{}, api.BatteryNormal, capacity, 0, between(0, 2000))
			case 2, 3, 4:
				bb[i].minPower = between(50, 200)
			}
		}

		total := between(-2000, 8000)
		ac := dispatchBatteryPower(bb, total)
		require.Len(t, ac, len(bb))

		var sum, sumMin, sumMax float64
		for i, b := range bb {
			require.True(t, isFinite(ac[i]), "finite")

			// only inverters with min power are stopped, the others share the total
			if b.minPower > 0 && ac[i] == 0 {
				continue
			}

			require.GreaterOrEqual(t, ac[i], b.minAC-1e-6, "min %v", b)
			require.LessOrEqual(t, ac[i], b.maxAC+1e-6, "max %v", b)

			if b.minPower > 0 {
				require.GreaterOrEqual(t, math.Abs(ac[i]), b.minPower-1e-6, "min power %v", b)
			}

			sum += ac[i]
			sumMin += b.minAC
			sumMax += b.maxAC
		}

		require.InDelta(t, max(sumMin, min(sumMax, total)), sum, 1e-6, "total")
	}
}
