package meter

import (
	"fmt"
	"testing"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/plugin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMarstekBatteryPowerControl(t *testing.T) {
	for _, tc := range []struct {
		workMode   any
		controlled bool
	}{
		{1, false},
		{"evcc", true},
	} {
		m, err := NewFromConfig(t.Context(), "template", map[string]any{
			"template":          "marstek-venus-a",
			"usage":             "battery",
			"modbus":            "tcpip",
			"host":              "192.0.2.2",
			"capacity":          5,
			"maxchargepower":    1500,
			"maxdischargepower": 800,
			"work_mode_normal":  tc.workMode,
		})
		require.NoError(t, err)

		assert.Equal(t, tc.controlled, api.HasCap[api.BatteryPowerController](m), "work mode %v", tc.workMode)
		assert.True(t, api.HasCap[api.BatteryController](m), "work mode %v", tc.workMode)
	}
}

func TestBatteryACPower(t *testing.T) {
	for _, tc := range []struct {
		name                                     string
		power, maxCharge, maxDischarge, minPower float64
		ac                                       float64
	}{
		{"discharge", 300, 1500, 800, 100, 300},
		{"discharge limited", 1100, 1500, 800, 100, 800},
		{"charge", -1000, 1500, 800, 100, -1000},
		{"charge limited", -2000, 1500, 800, 100, -1500},
		{"below min power stops", 60, 1500, 800, 100, 0},
		{"charge below min power stops", -60, 1500, 800, 100, 0},
		{"1W resolution", 304.4, 0, 0, 0, 304},
		{"unlimited", 3000, 0, 0, 0, 3000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.ac, batteryACPower(tc.power, tc.maxCharge, tc.maxDischarge, tc.minPower))
		})
	}
}

func TestBatteryPowerController(t *testing.T) {
	// the setters only accept 500W charge and 300W discharge, so errors show where a setpoint went
	setter := func(param string, val int) *plugin.Config {
		return &plugin.Config{Source: "go", Other: map[string]any{"script": fmt.Sprintf("[]int{0}[%s-%d]", param, val)}}
	}

	t.Run("not configured", func(t *testing.T) {
		var cc batteryPowerCtx
		pvG, minG, setS, err := cc.Controller(t.Context(), nil)
		require.NoError(t, err)
		assert.Nil(t, pvG)
		assert.Nil(t, minG)
		assert.Nil(t, setS)
	})

	t.Run("charge or discharge missing", func(t *testing.T) {
		for _, cc := range []batteryPowerCtx{
			{ChargePower: setter("chargePower", 500)},
			{DischargePower: setter("dischargePower", 300)},
		} {
			_, _, _, err := cc.Controller(t.Context(), nil)
			assert.Error(t, err)
		}
	})

	t.Run("without pv", func(t *testing.T) {
		cc := batteryPowerCtx{ChargePower: setter("chargePower", 500), DischargePower: setter("dischargePower", 300), MinPower: 100}
		pvG, minG, setS, err := cc.Controller(t.Context(), nil)
		require.NoError(t, err)

		pv, err := pvG()
		require.NoError(t, err)
		assert.Zero(t, pv)
		assert.Equal(t, 100.0, minG())

		assert.NoError(t, setS(-500), "charge")
		assert.NoError(t, setS(300), "discharge")
		assert.Error(t, setS(-300), "charge")
		assert.Error(t, setS(500), "discharge")
	})

	t.Run("with pv and limits", func(t *testing.T) {
		cc := batteryPowerCtx{
			ChargePower:    setter("chargePower", 500),
			DischargePower: setter("dischargePower", 300),
			PvPower:        &plugin.Config{Source: "const", Other: map[string]any{"value": "250"}},
		}
		limits := func() (float64, float64, error) { return 500, 300, nil }
		pvG, _, setS, err := cc.Controller(t.Context(), limits)
		require.NoError(t, err)

		pv, err := pvG()
		require.NoError(t, err)
		assert.Equal(t, 250.0, pv)

		assert.NoError(t, setS(-2000), "charge clamped")
		assert.NoError(t, setS(2000), "discharge clamped")
	})
}
