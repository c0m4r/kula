package collector

import (
	"testing"
	"time"

	"kula/internal/config"
)

// Power supplies are read at collection.power_supply_interval: the samples
// between repeat the last reading, each holding a copy of its own.
func TestPowerSuppliesAtTheirOwnInterval(t *testing.T) {
	oldSys := sysPath
	sysPath = t.TempDir()
	t.Cleanup(func() { sysPath = oldSys })
	c := New(config.GlobalConfig{}, config.CollectionConfig{Interval: time.Second}, config.ApplicationsConfig{}, "")
	battery := func(capacity string) {
		diskTestFile(t, sysPath, "class/power_supply/BAT1/type", "Battery\n")
		diskTestFile(t, sysPath, "class/power_supply/BAT1/status", "Discharging\n")
		diskTestFile(t, sysPath, "class/power_supply/BAT1/capacity", capacity)
	}

	battery("80\n")
	first := c.powerSupplies()
	if len(first) != 1 || first[0].Name != "BAT1" || first[0].Status != "Discharging" || first[0].Capacity != 80 {
		t.Fatalf("first reading: %+v", first)
	}
	battery("79\n")
	first[0].Capacity = 0
	if got := c.powerSupplies(); len(got) != 1 || got[0].Capacity != 80 {
		t.Fatalf("second reading: %+v, want the first repeated, unaffected by a change to it", got)
	}
}
