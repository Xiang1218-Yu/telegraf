//go:build linux

package systemd_units

import (
	"context"
	"sort"
	"strings"

	"github.com/coreos/go-systemd/v22/dbus"

	"github.com/influxdata/telegraf"
)

// Health states reported by the 'systemd_unit_health' metric.
const (
	healthHealthy = iota
	healthSelfFailed
	healthDependencyFailed
	healthDependencyMissing
	healthDependencyCycle
	healthIncomplete
)

var healthNames = map[int]string{
	healthHealthy:           "healthy",
	healthSelfFailed:        "self-failed",
	healthDependencyFailed:  "dependency-failed",
	healthDependencyMissing: "dependency-missing",
	healthDependencyCycle:   "dependency-cycle",
	healthIncomplete:        "incomplete",
}

// unitDependencies holds the forward ('Requires', 'Wants') and reverse
// ('RequiredBy', 'WantedBy') dependency lists of a single unit as exposed by
// the org.freedesktop.systemd1.Unit D-Bus properties.
type unitDependencies struct {
	requires   []string
	wants      []string
	requiredBy []string
	wantedBy   []string
}

// forward returns the de-duplicated union of the 'Requires' and 'Wants'
// dependencies, i.e. the units this unit depends on.
func (d *unitDependencies) forward() []string {
	return union(d.requires, d.wants)
}

// reverse returns the de-duplicated union of the 'RequiredBy' and
// 'WantedBy' dependencies, i.e. the units depending on this unit.
func (d *unitDependencies) reverse() []string {
	return union(d.requiredBy, d.wantedBy)
}

func union(lists ...[]string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, list := range lists {
		for _, name := range list {
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// dependencyGraph resolves the dependency relationships of the collected
// units. The graph is rebuilt from scratch for every Gather cycle, thus
// changes of the dependency relationships (e.g. edited unit files or
// instantiated units) are reflected in the next result.
type dependencyGraph struct {
	client client
	ctx    context.Context
	log    telegraf.Logger

	// Status information, indexed by unit name. 'hasStatus' is false for
	// units whose status could not be resolved, which prevents certifying
	// the health of any unit depending on them.
	statuses  map[string]dbus.UnitStatus
	hasStatus map[string]bool

	// Dependency information, indexed by unit name. 'hasDeps' is false for
	// units whose dependency properties could not be read.
	deps    map[string]*unitDependencies
	hasDeps map[string]bool

	// warned keeps track of units for which a property-read warning was
	// already logged to avoid spamming the log on every gather cycle.
	warned map[string]bool
}

func newDependencyGraph(ctx context.Context, c client, roots []dbus.UnitStatus, log telegraf.Logger, warned map[string]bool) *dependencyGraph {
	g := &dependencyGraph{
		client:    c,
		ctx:       ctx,
		log:       log,
		statuses:  make(map[string]dbus.UnitStatus),
		hasStatus: make(map[string]bool),
		deps:      make(map[string]*unitDependencies),
		hasDeps:   make(map[string]bool),
		warned:    warned,
	}

	// Seed the graph with the authoritative states of the collected units.
	for _, st := range roots {
		g.statuses[st.Name] = st
		g.hasStatus[st.Name] = true
	}

	return g
}

// build traverses the forward dependencies of all roots, resolving unit
// states and dependency properties. All information is cached, so roots
// sharing parts of the graph only trigger a single set of D-Bus calls.
func (g *dependencyGraph) build(roots []string) {
	frontier := uniqueNames(roots)
	visited := make(map[string]bool, len(frontier))
	for _, name := range frontier {
		visited[name] = true
	}

	for len(frontier) > 0 {
		// Resolve the state of all units on the frontier in a single call.
		g.resolveStatuses(frontier)

		var next []string
		for _, name := range frontier {
			// A unit whose state cannot be resolved makes any result
			// depending on it incomplete; there is nothing to traverse.
			if !g.hasStatus[name] {
				continue
			}

			// Missing units have no readable properties and no edges.
			if g.statuses[name].LoadState == "not-found" {
				continue
			}

			// Read the dependency properties, follow the forward edges.
			d := g.readDependencies(name)
			if d == nil {
				continue
			}
			for _, dep := range d.forward() {
				if !visited[dep] {
					visited[dep] = true
					next = append(next, dep)
				}
			}
		}
		frontier = next
	}
}

// resolveStatuses resolves the state of units not already present in the
// cache using a single ListUnitsByNames call. systemd returns an entry for
// every requested name, even for non-existing ones (marked 'not-found').
func (g *dependencyGraph) resolveStatuses(names []string) {
	var unknown []string
	for _, name := range names {
		if _, ok := g.hasStatus[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) == 0 {
		return
	}

	resolved, err := g.client.ListUnitsByNamesContext(g.ctx, unknown)
	if err != nil {
		// Mark all names as unresolvable so dependent units are reported
		// as incomplete instead of being misjudged as healthy.
		for _, name := range unknown {
			g.hasStatus[name] = false
		}
		if !g.warned["__status__"] {
			g.log.Warnf("Cannot resolve unit states: %v", err)
			g.warned["__status__"] = true
		}
		return
	}

	for _, st := range resolved {
		g.statuses[st.Name] = st
		g.hasStatus[st.Name] = true
	}

	// Be defensive about daemons omitting entries: a name without a result
	// behaves like a non-existing unit.
	for _, name := range unknown {
		if !g.hasStatus[name] {
			g.statuses[name] = dbus.UnitStatus{
				Name:        name,
				LoadState:   "not-found",
				ActiveState: "inactive",
				SubState:    "dead",
			}
			g.hasStatus[name] = true
		}
	}
}

// readDependencies reads and caches the Requires/Wants/RequiredBy/WantedBy
// lists of a unit. A nil result with hasDeps set to false marks the unit as
// unreadable.
func (g *dependencyGraph) readDependencies(name string) *unitDependencies {
	if d, ok := g.deps[name]; ok {
		return d
	}
	if attempted, ok := g.hasDeps[name]; ok && attempted {
		return nil
	}

	props, err := g.client.GetUnitPropertiesContext(g.ctx, name)
	if err != nil {
		g.hasDeps[name] = false
		if !g.warned[name] {
			g.log.Warnf("Cannot read dependency properties for %q: %v", name, err)
			g.warned[name] = true
		}
		return nil
	}

	d := &unitDependencies{
		requires:   toStringSlice(props["Requires"]),
		wants:      toStringSlice(props["Wants"]),
		requiredBy: toStringSlice(props["RequiredBy"]),
		wantedBy:   toStringSlice(props["WantedBy"]),
	}
	g.deps[name] = d
	g.hasDeps[name] = true

	return d
}

// analyze computes the health summary for a single collected unit.
func (g *dependencyGraph) analyze(root string) (int, map[string]any) {
	fields := map[string]any{
		"requires_total":       0,
		"wants_total":          0,
		"reverse_total":        0,
		"failed_dependencies":  "",
		"missing_dependencies": "",
		"cyclic_dependencies":  "",
		"graph_complete":       true,
		"self_failed":          false,
	}

	selfFailed := false
	if rootState, ok := g.statuses[root]; ok {
		selfFailed = unitFailed(rootState)
	}
	fields["self_failed"] = selfFailed

	complete := true

	// The root unit's own properties provide the direct dependency counts.
	rootDeps, rootRead := g.deps[root], g.hasDeps[root]
	if !rootRead {
		complete = false
	} else {
		fields["requires_total"] = len(rootDeps.requires)
		fields["wants_total"] = len(rootDeps.wants)
		fields["reverse_total"] = len(rootDeps.reverse())
	}

	// Walk all reachable forward dependencies and classify them.
	reachable := map[string]bool{root: true}
	failed := make(map[string]bool)
	missing := make(map[string]bool)
	queue := []string{root}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if current != root {
			if !g.hasStatus[current] {
				// State of a reachable dependency is unknown.
				complete = false
			} else {
				switch st := g.statuses[current]; {
				case st.LoadState == "not-found":
					// A known missing unit has no dependencies to follow
					// and must not mark the graph as incomplete.
					missing[current] = true
					continue
				case unitFailed(st):
					failed[current] = true
				}
			}
		}

		if !g.hasDeps[current] {
			// Dependency relationships of a reachable unit are unknown,
			// so the graph (and any health verdict based on it) is
			// incomplete.
			complete = false
			continue
		}

		for _, dep := range g.deps[current].forward() {
			if !reachable[dep] {
				reachable[dep] = true
				queue = append(queue, dep)
			}
		}
	}

	// Detect cycles within the reachable graph.
	cyclic := findCycleMembers(root, g.deps, reachable)

	fields["graph_complete"] = complete
	fields["failed_dependencies"] = joinNames(failed)
	fields["missing_dependencies"] = joinNames(missing)
	fields["cyclic_dependencies"] = joinNames(cyclic)

	// Determine the verdict with a fixed precedence. An incomplete graph
	// always wins: a service whose dependency graph cannot be read must
	// never be reported as healthy.
	switch {
	case !complete:
		return healthIncomplete, fields
	case selfFailed:
		return healthSelfFailed, fields
	case len(cyclic) > 0:
		return healthDependencyCycle, fields
	case len(missing) > 0:
		return healthDependencyMissing, fields
	case len(failed) > 0:
		return healthDependencyFailed, fields
	default:
		return healthHealthy, fields
	}
}

// findCycleMembers returns the names participating in dependency cycles
// reachable from root using a depth-first search with tri-color marking.
func findCycleMembers(root string, deps map[string]*unitDependencies, reachable map[string]bool) map[string]bool {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int)
	var stack []string
	members := make(map[string]bool)

	var visit func(string)
	visit = func(node string) {
		color[node] = gray
		stack = append(stack, node)

		if d, ok := deps[node]; ok {
			for _, next := range d.forward() {
				if !reachable[next] {
					continue
				}
				switch color[next] {
				case gray:
					// Back-edge: everything from 'next' to the top of the
					// DFS stack participates in the cycle.
					for i := indexOf(stack, next); i >= 0 && i < len(stack); i++ {
						members[stack[i]] = true
					}
				case white:
					visit(next)
				}
			}
		}

		stack = stack[:len(stack)-1]
		color[node] = black
	}

	visit(root)

	return members
}

// unitFailed reports whether the state of a unit itself indicates a failure,
// i.e. the unit is marked failed or could not be loaded due to an error.
func unitFailed(st dbus.UnitStatus) bool {
	if st.ActiveState == "failed" {
		return true
	}
	switch st.LoadState {
	case "error", "bad-setting", "masked":
		return true
	}
	return false
}

func toStringSlice(value any) []string {
	values, ok := value.([]string)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func uniqueNames(names []string) []string {
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, name := range names {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

func indexOf(list []string, name string) int {
	for i, v := range list {
		if v == name {
			return i
		}
	}
	return -1
}

func joinNames(names map[string]bool) string {
	if len(names) == 0 {
		return ""
	}
	list := make([]string, 0, len(names))
	for name := range names {
		list = append(list, name)
	}
	sort.Strings(list)
	return strings.Join(list, ",")
}
