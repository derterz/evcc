package meter

import (
	"testing"

	"github.com/evcc-io/evcc/api"
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
		name                                         string
		power, pv, maxCharge, maxDischarge, minPower float64
		ac                                           float64
	}{
		{"discharge", 300, 0, 1500, 800, 100, 300},
		{"discharge plus own pv", 300, 200, 1500, 800, 100, 500},
		{"discharge limited", 700, 400, 1500, 800, 100, 800},
		{"hold passes own pv through", 0, 250, 1500, 800, 100, 250},
		{"charge", -1000, 0, 1500, 800, 100, -1000},
		{"charge offset by own pv", -1000, 400, 1500, 800, 100, -600},
		{"charge limited", -2000, 0, 1500, 800, 100, -1500},
		{"below min power stops", 60, 0, 1500, 800, 100, 0},
		{"rounded to 10W", 304, 0, 0, 0, 0, 300},
		{"unlimited", 3000, 0, 0, 0, 0, 3000},
		{"negative pv ignored", 300, -20, 0, 0, 0, 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.ac, batteryACPower(tc.power, tc.pv, tc.maxCharge, tc.maxDischarge, tc.minPower))
		})
	}
}
