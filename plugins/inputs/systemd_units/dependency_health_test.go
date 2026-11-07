//go:build linux

package systemd_units

import (
	"testing"
	"time"

	sdbus "github.com/coreos/go-systemd/v22/dbus"
	"github.com/stretchr/testify/require"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/config"
	"github.com/influxdata/telegraf/metric"
	"github.com/influxdata/telegraf/testutil"
)

func unitStatus(name, load, active, sub string) *sdbus.UnitStatus {
	return &sdbus.UnitStatus{
		Name:        name,
		LoadState:   load,
		ActiveState: active,
		SubState:    sub,
	}
}

func setupHealthPlugin(t *testing.T, units map[string]properties) (*SystemdUnits, *fakeClient) {
	t.Helper()

	plugin := &SystemdUnits{
		Pattern:                 "app*",
		CollectDependencyHealth: true,
		Timeout:                 config.Duration(time.Second),
		Log:                     testutil.Logger{},
	}
	require.NoError(t, plugin.Init())

	client := &fakeClient{
		units:     units,
		connected: true,
	}
	client.fixPropertyTypes()
	plugin.client = client
	t.Cleanup(plugin.Stop)

	return plugin, client
}

// healthFields builds the expected fields of a health metric, pre-filled
// with the default (healthy / no problems) values.
func healthFields(code int) map[string]any {
	return map[string]any{
		"health_code":          code,
		"requires_total":       0,
		"wants_total":          0,
		"reverse_total":        0,
		"failed_dependencies":  "",
		"missing_dependencies": "",
		"cyclic_dependencies":  "",
		"graph_complete":       true,
		"self_failed":          false,
	}
}

func healthMetric(name, health string, fields map[string]any) telegraf.Metric {
	return metric.New(
		"systemd_unit_health",
		map[string]string{"name": name, "health": health},
		fields,
		time.Unix(0, 0),
	)
}

// appUnitMetric is the regular systemd_units metric of a healthy
// 'app.service', expected alongside every health summary in these tests.
func appUnitMetric() telegraf.Metric {
	return unitMetric(unitStatus("app.service", "loaded", "active", "running"))
}

// unitMetric builds the expected regular systemd_units metric for a state.
func unitMetric(st *sdbus.UnitStatus) telegraf.Metric {
	return metric.New(
		"systemd_units",
		map[string]string{
			"name":   st.Name,
			"load":   st.LoadState,
			"active": st.ActiveState,
			"sub":    st.SubState,
		},
		map[string]any{
			"load_code":   loadMap[st.LoadState],
			"active_code": activeMap[st.ActiveState],
			"sub_code":    subMap[st.SubState],
		},
		time.Unix(0, 0),
	)
}

func healthyApp() properties {
	return properties{
		utype: "Service",
		state: unitStatus("app.service", "loaded", "active", "running"),
	}
}

func TestDependencyHealth(t *testing.T) {
	tests := []struct {
		name     string
		units    map[string]properties
		root     properties
		expected map[string]any
		health   string
	}{
		{
			name:   "healthy with requires wants and reverse dependencies",
			health: "healthy",
			root: func() properties {
				u := healthyApp()
				u.requires = []string{"db.service"}
				u.wants = []string{"logger.service"}
				u.requiredBy = []string{"client.service"}
				u.wantedBy = []string{"other.service", "client.service"}
				return u
			}(),
			units: map[string]properties{
				"db.service": {
					state: unitStatus("db.service", "loaded", "active", "running"),
				},
				"logger.service": {
					state: unitStatus("logger.service", "loaded", "active", "running"),
				},
			},
			expected: func() map[string]any {
				f := healthFields(healthHealthy)
				f["requires_total"] = 1
				f["wants_total"] = 1
				f["reverse_total"] = 2
				return f
			}(),
		},
		{
			name:   "self failure takes precedence over healthy dependencies",
			health: "self-failed",
			root: func() properties {
				u := properties{
					utype: "Service",
					state: unitStatus("app.service", "loaded", "failed", "failed"),
				}
				u.requires = []string{"db.service"}
				return u
			}(),
			units: map[string]properties{
				"db.service": {
					state: unitStatus("db.service", "loaded", "active", "running"),
				},
			},
			expected: func() map[string]any {
				f := healthFields(healthSelfFailed)
				f["requires_total"] = 1
				f["self_failed"] = true
				return f
			}(),
		},
		{
			name:   "direct dependency failure",
			health: "dependency-failed",
			root: func() properties {
				u := healthyApp()
				u.requires = []string{"db.service"}
				return u
			}(),
			units: map[string]properties{
				"db.service": {
					state: unitStatus("db.service", "loaded", "failed", "failed"),
				},
			},
			expected: func() map[string]any {
				f := healthFields(healthDependencyFailed)
				f["requires_total"] = 1
				f["failed_dependencies"] = "db.service"
				return f
			}(),
		},
		{
			name:   "transitive dependency failure",
			health: "dependency-failed",
			root: func() properties {
				u := healthyApp()
				u.requires = []string{"stack.target"}
				return u
			}(),
			units: map[string]properties{
				"stack.target": {
					state:    unitStatus("stack.target", "loaded", "active", "active"),
					requires: []string{"db.service"},
				},
				"db.service": {
					state: unitStatus("db.service", "loaded", "failed", "failed"),
				},
			},
			expected: func() map[string]any {
				f := healthFields(healthDependencyFailed)
				f["requires_total"] = 1
				f["failed_dependencies"] = "db.service"
				return f
			}(),
		},
		{
			name:   "failed wants dependency",
			health: "dependency-failed",
			root: func() properties {
				u := healthyApp()
				u.wants = []string{"weak.service"}
				return u
			}(),
			units: map[string]properties{
				"weak.service": {
					state: unitStatus("weak.service", "loaded", "failed", "failed"),
				},
			},
			expected: func() map[string]any {
				f := healthFields(healthDependencyFailed)
				f["wants_total"] = 1
				f["failed_dependencies"] = "weak.service"
				return f
			}(),
		},
		{
			name:   "missing dependency",
			health: "dependency-missing",
			root: func() properties {
				u := healthyApp()
				u.requires = []string{"ghost.service"}
				return u
			}(),
			units: map[string]properties{},
			expected: func() map[string]any {
				f := healthFields(healthDependencyMissing)
				f["requires_total"] = 1
				f["missing_dependencies"] = "ghost.service"
				return f
			}(),
		},
		{
			name:   "dependency cycle",
			health: "dependency-cycle",
			root: func() properties {
				u := healthyApp()
				u.requires = []string{"a.service"}
				return u
			}(),
			units: map[string]properties{
				"a.service": {
					state:    unitStatus("a.service", "loaded", "active", "running"),
					requires: []string{"b.service"},
				},
				"b.service": {
					state:    unitStatus("b.service", "loaded", "active", "running"),
					requires: []string{"a.service"},
				},
			},
			expected: func() map[string]any {
				f := healthFields(healthDependencyCycle)
				f["requires_total"] = 1
				f["cyclic_dependencies"] = "a.service,b.service"
				return f
			}(),
		},
		{
			name:   "self loop is a cycle",
			health: "dependency-cycle",
			root: func() properties {
				u := healthyApp()
				u.requires = []string{"loop.service"}
				return u
			}(),
			units: map[string]properties{
				"loop.service": {
					state:    unitStatus("loop.service", "loaded", "active", "running"),
					requires: []string{"loop.service"},
				},
			},
			expected: func() map[string]any {
				f := healthFields(healthDependencyCycle)
				f["requires_total"] = 1
				f["cyclic_dependencies"] = "loop.service"
				return f
			}(),
		},
		{
			name:   "unreadable dependency graph is incomplete",
			health: "incomplete",
			root: func() properties {
				u := healthyApp()
				u.requires = []string{"broken.service"}
				return u
			}(),
			units: map[string]properties{
				"broken.service": {
					// Unit is loaded and healthy, but its dependency
					// properties cannot be read.
					state:        unitStatus("broken.service", "loaded", "active", "running"),
					unitPropsErr: true,
				},
			},
			expected: func() map[string]any {
				f := healthFields(healthIncomplete)
				f["requires_total"] = 1
				f["graph_complete"] = false
				return f
			}(),
		},
		{
			name:   "unreadable root properties are incomplete",
			health: "incomplete",
			root: func() properties {
				u := healthyApp()
				u.requires = []string{"db.service"}
				u.unitPropsErr = true
				return u
			}(),
			units: map[string]properties{
				"db.service": {
					state: unitStatus("db.service", "loaded", "active", "running"),
				},
			},
			expected: func() map[string]any {
				f := healthFields(healthIncomplete)
				f["graph_complete"] = false
				return f
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.units["app.service"] = tt.root
			plugin, _ := setupHealthPlugin(t, tt.units)

			var acc testutil.Accumulator
			require.NoError(t, acc.GatherError(plugin.Gather))

			expected := []telegraf.Metric{
				unitMetric(tt.root.state),
				healthMetric("app.service", tt.health, tt.expected),
			}
			testutil.RequireMetricsEqual(t, expected, acc.GetTelegrafMetrics(), testutil.IgnoreTime())
		})
	}
}

func TestDependencyHealthUpdatesOnChanges(t *testing.T) {
	units := map[string]properties{
		"app.service": func() properties {
			u := healthyApp()
			u.requires = []string{"db.service"}
			return u
		}(),
		"db.service": {
			state: unitStatus("db.service", "loaded", "active", "running"),
		},
	}
	plugin, client := setupHealthPlugin(t, units)

	// First gather: everything is healthy.
	var acc testutil.Accumulator
	require.NoError(t, acc.GatherError(plugin.Gather))
	healthyFields := healthFields(healthHealthy)
	healthyFields["requires_total"] = 1
	testutil.RequireMetricsEqual(
		t,
		[]telegraf.Metric{
			appUnitMetric(),
			healthMetric("app.service", "healthy", healthyFields),
		},
		acc.GetTelegrafMetrics(),
		testutil.IgnoreTime(),
	)

	// The dependency fails: the rebuilt graph must report it.
	db := client.units["db.service"]
	db.state = unitStatus("db.service", "loaded", "failed", "failed")
	client.units["db.service"] = db

	acc = testutil.Accumulator{}
	require.NoError(t, acc.GatherError(plugin.Gather))
	failedFields := healthFields(healthDependencyFailed)
	failedFields["requires_total"] = 1
	failedFields["failed_dependencies"] = "db.service"
	testutil.RequireMetricsEqual(
		t,
		[]telegraf.Metric{
			appUnitMetric(),
			healthMetric("app.service", "dependency-failed", failedFields),
		},
		acc.GetTelegrafMetrics(),
		testutil.IgnoreTime(),
	)

	// The dependency relationship changes: db is gone, a missing unit is
	// required instead. The next result must reflect the new graph.
	app := client.units["app.service"]
	app.requires = []string{"ghost.service"}
	client.units["app.service"] = app

	acc = testutil.Accumulator{}
	require.NoError(t, acc.GatherError(plugin.Gather))
	missingFields := healthFields(healthDependencyMissing)
	missingFields["requires_total"] = 1
	missingFields["missing_dependencies"] = "ghost.service"
	testutil.RequireMetricsEqual(
		t,
		[]telegraf.Metric{
			appUnitMetric(),
			healthMetric("app.service", "dependency-missing", missingFields),
		},
		acc.GetTelegrafMetrics(),
		testutil.IgnoreTime(),
	)
}

func TestDependencyHealthDisabledKeepsMetricsUnchanged(t *testing.T) {
	plugin := &SystemdUnits{
		Pattern: "app*",
		Timeout: config.Duration(time.Second),
		Log:     testutil.Logger{},
	}
	require.NoError(t, plugin.Init())

	client := &fakeClient{
		units: map[string]properties{
			"app.service": func() properties {
				u := healthyApp()
				u.requires = []string{"db.service"}
				return u
			}(),
			"db.service": {
				state: unitStatus("db.service", "loaded", "failed", "failed"),
			},
		},
		connected: true,
	}
	client.fixPropertyTypes()
	plugin.client = client
	t.Cleanup(plugin.Stop)

	var acc testutil.Accumulator
	require.NoError(t, acc.GatherError(plugin.Gather))

	metrics := acc.GetTelegrafMetrics()
	testutil.RequireMetricsEqual(
		t,
		[]telegraf.Metric{appUnitMetric()},
		metrics,
		testutil.IgnoreTime(),
	)
	for _, m := range metrics {
		require.NotEqual(t, "systemd_unit_health", m.Name())
	}
}
