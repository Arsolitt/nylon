package state

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
)

func TestParseGraph_SimpleGraph(t *testing.T) {
	nodes := []string{"1", "2", "3", "4", "5"}
	input := `1, 2
3, 4
1,3,5`
	pairs, err := ParseGraph(strings.Split(input, "\n"), nodes)
	assert.NoError(t, err)
	assert.ElementsMatch(t, pairs, []Pair[NodeId, NodeId]{
		{"1", "2"},
		{"3", "4"},
		{"1", "3"},
		{"3", "5"},
		{"1", "5"},
	})
}

func TestParseGraph_Groups(t *testing.T) {
	nodes := []string{"1", "2", "3", "4", "5", "6", "7"}
	input := `a = 1,2
b=3,,,4
c=5,6
d=a,b
d,d
7,d`
	pairs, err := ParseGraph(strings.Split(input, "\n"), nodes)
	assert.NoError(t, err)
	assert.ElementsMatch(t, pairs, []Pair[NodeId, NodeId]{
		// d,d
		{"1", "2"},
		{"1", "3"},
		{"1", "4"},
		{"2", "3"},
		{"2", "4"},
		{"3", "4"},
		// 7,d
		{"1", "7"},
		{"2", "7"},
		{"3", "7"},
		{"4", "7"},
	})
}

func TestParseGraph_Cycle(t *testing.T) {
	nodes := []string{}
	input := `a = b
b = c
c = a`
	_, err := ParseGraph(strings.Split(input, "\n"), nodes)
	assert.ErrorContains(t, err, "cycle detected in graph: [a b c]")
}

func TestParseGraph_DupGroupName(t *testing.T) {
	nodes := []string{}
	input := `a = b
a = b
b = b`
	_, err := ParseGraph(strings.Split(input, "\n"), nodes)
	assert.ErrorContains(t, err, "duplicate group name: a")
}

func TestParseGraph_SymbolError(t *testing.T) {
	nodes := []string{"1"}
	input := `a = 1
b = 2`
	_, err := ParseGraph(strings.Split(input, "\n"), nodes)
	assert.ErrorContains(t, err, "2 is not a valid node/group")
}

func TestParseGraph_EmptyGroup(t *testing.T) {
	nodes := []string{"1"}
	input := `a =`
	_, err := ParseGraph(strings.Split(input, "\n"), nodes)
	assert.ErrorContains(t, err, "node/group list must not be empty")
}

func TestParseGraph_GroupNameIsNodeName(t *testing.T) {
	nodes := []string{"1"}
	input := `1 = 1`
	_, err := ParseGraph(strings.Split(input, "\n"), nodes)
	assert.ErrorContains(t, err, "group name must not be a node name: 1")
}

func TestParseGraph_InvalidGroupDefinition(t *testing.T) {
	nodes := []string{"1"}
	input := `a = 1 = b`
	_, err := ParseGraph(strings.Split(input, "\n"), nodes)
	assert.ErrorContains(t, err, ". group definition must contain one '='")
}

func TestParseGraph_Single(t *testing.T) {
	nodes := []string{"1", "2", "3", "4", "5"}
	input := `1`
	_, err := ParseGraph(strings.Split(input, "\n"), nodes)
	assert.ErrorContains(t, err, "invalid pairing, [1]")
}

func TestParseGraph_None(t *testing.T) {
	nodes := []string{"1", "2", "3", "4", "5"}
	input := ``
	_, err := ParseGraph(strings.Split(input, "\n"), nodes)
	assert.ErrorContains(t, err, "node/group list must not be empty")
}

func TestParseGraph_GroupsDeep(t *testing.T) {
	nodes := []string{"1", "2", "3", "4", "5", "6", "7"}
	input := `a = 1,2
b = a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a
c = a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a
d = a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a
e = a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a
f = a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a
g = a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a
h = a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a
i = a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a
j = a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a
k = a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a
k,k,3`
	pairs, err := ParseGraph(strings.Split(input, "\n"), nodes)
	assert.NoError(t, err)
	assert.ElementsMatch(t, pairs, []Pair[NodeId, NodeId]{
		{"1", "2"},
		{"1", "3"},
		{"2", "3"},
	})
}

func failGraph(t *testing.T, graph string) {
	_, err := ParseGraph(strings.Split(graph, "\n"), []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10"})
	assert.Error(t, err)
}

func TestParseGraph_InvalidGraph(t *testing.T) {
	failGraph(t, `this graph is a baddie`)
	failGraph(t, `=========,,,,`)
	failGraph(t, `#`)
	failGraph(t, `\n\n\n\n\n\n`)
	failGraph(t, `1`)
	failGraph(t, `1,2,3,4,5,6,a`)
	failGraph(t, `1,2,3,4,5,6,7,8,9,10,11,12,13,14,15`)
	failGraph(t, `,,,,,,,,,,,,,,,,`)
	failGraph(t, `a=a`)
}

func TestExpandCentralConfigIsIdempotent(t *testing.T) {
	addr := netip.MustParseAddr("192.0.2.1")
	cfg := CentralCfg{
		Routers: []RouterCfg{{
			NodeCfg: NodeCfg{
				Id:        "router",
				Addresses: []netip.Addr{addr},
			},
		}},
	}

	ExpandCentralConfig(&cfg)
	ExpandCentralConfig(&cfg)

	if assert.Len(t, cfg.Routers[0].Prefixes, 1) {
		assert.Equal(t, AddrToPrefix(addr), cfg.Routers[0].Prefixes[0].GetPrefix())
	}
}

func TestTunablesOverridesParse(t *testing.T) {
	var cfg LocalCfg
	src := `
router_tunables:
  probe_delay: 1500ms
  probe_recovery_delay: 2s
  probe_discovery_delay: 15s
  gc_delay: 2s
  link_dead_threshold: 8s
  probe_timeout: 3s
  route_expiry_time: 30s
  route_switch_hold: 4s
  no_endpoint_warn_interval: 2h
  handshake_stale_warn: 130s
  handshake_stale_demote: 145s
  handshake_stale_debounce: 10s
  handshake_stale_warn_interval: 30m
  loss_penalty_gain: 0.9
  loss_demotion_threshold: 0.5
  link_switch_deadband: 1.25
  hop_cost: 7
`
	assert.NoError(t, yaml.Unmarshal([]byte(src), &cfg))
	tr := cfg.Tunables
	if assert.NotNil(t, tr) {
		assert.Equal(t, 1500*time.Millisecond, *tr.ProbeDelay)
		assert.Equal(t, 2*time.Second, *tr.ProbeRecoveryDelay)
		assert.Equal(t, 15*time.Second, *tr.ProbeDiscoveryDelay)
		assert.Equal(t, 2*time.Second, *tr.GcDelay)
		assert.Equal(t, 8*time.Second, *tr.LinkDeadThreshold)
		assert.Equal(t, 3*time.Second, *tr.ProbeTimeout)
		assert.Equal(t, 30*time.Second, *tr.RouteExpiryTime)
		assert.Equal(t, 4*time.Second, *tr.RouteSwitchHold)
		assert.Equal(t, 2*time.Hour, *tr.NoEndpointWarnInterval)
		assert.Equal(t, 130*time.Second, *tr.HandshakeStaleWarn)
		assert.Equal(t, 145*time.Second, *tr.HandshakeStaleDemote)
		assert.Equal(t, 10*time.Second, *tr.HandshakeStaleDebounce)
		assert.Equal(t, 30*time.Minute, *tr.HandshakeStaleWarnInterval)
		assert.Equal(t, 0.9, *tr.LossPenaltyGain)
		assert.Equal(t, 0.5, *tr.LossDemotionThreshold)
		assert.Equal(t, 1.25, *tr.LinkSwitchDeadband)
		assert.Equal(t, uint32(7), *tr.HopCost)
	}
}

func TestTunablesOverridesAbsentNil(t *testing.T) {
	var cfg LocalCfg
	assert.NoError(t, yaml.Unmarshal([]byte("id: node\nport: 1\n"), &cfg))
	assert.Nil(t, cfg.Tunables)
	def := DefaultRouterTunables()
	ApplyTunablesOverrides(&def, cfg.Tunables)
	assert.Equal(t, DefaultRouterTunables(), def)
}

func TestTunablesOverridesRejected(t *testing.T) {
	cases := []string{
		"router_tunables:\n  probe_delay: 0s\n",
		"router_tunables:\n  probe_recovery_delay: -1s\n",
		"router_tunables:\n  probe_discovery_delay: 0s\n",
		"router_tunables:\n  gc_delay: -1s\n",
		"router_tunables:\n  route_expiry_time: 0s\n",
		"router_tunables:\n  no_endpoint_warn_interval: 0s\n",
		"router_tunables:\n  handshake_stale_debounce: 0s\n",
		"router_tunables:\n  handshake_stale_warn_interval: -1s\n",
		"router_tunables:\n  probe_timeout: 11s\n",
		"router_tunables:\n  probe_timeout: -1s\n",
		"router_tunables:\n  loss_penalty_gain: 11\n",
		"router_tunables:\n  loss_penalty_gain: -0.1\n",
		"router_tunables:\n  loss_demotion_threshold: 1\n",
		"router_tunables:\n  loss_demotion_threshold: -0.1\n",
		"router_tunables:\n  link_switch_deadband: 0.9\n",
		"router_tunables:\n  route_switch_hold: -1s\n",
		"router_tunables:\n  handshake_stale_warn: 151s\n  handshake_stale_demote: 150s\n",
		"router_tunables:\n  handshake_stale_warn: 160s\n",
		"router_tunables:\n  link_dead_threshold: 1ms\n",
	}
	for _, src := range cases {
		var cfg LocalCfg
		if !assert.NoError(t, yaml.Unmarshal([]byte(src), &cfg), src) {
			continue
		}
		node := LocalCfg{Id: "node", Port: 1, Key: NyPrivateKey{1}}
		node.Tunables = cfg.Tunables
		assert.Error(t, NodeConfigValidator(nil, &node), src)
	}
}

func TestTunablesOverridesHappyPath(t *testing.T) {
	var cfg LocalCfg
	src := "router_tunables:\n  probe_timeout: 5s\n  loss_demotion_threshold: 0.25\n"
	assert.NoError(t, yaml.Unmarshal([]byte(src), &cfg))
	node := LocalCfg{Id: "node", Port: 1, Key: NyPrivateKey{1}}
	node.Tunables = cfg.Tunables
	assert.NoError(t, NodeConfigValidator(nil, &node))
	def := DefaultRouterTunables()
	ApplyTunablesOverrides(&def, cfg.Tunables)
	assert.Equal(t, 5*time.Second, def.ProbeTimeout)
	assert.Equal(t, 0.25, def.LossDemotionThreshold)
	assert.Equal(t, DefaultRouterTunables().LossPenaltyGain, def.LossPenaltyGain)
	var cfg2 LocalCfg
	src2 := "router_tunables:\n  handshake_stale_warn: 160s\n  handshake_stale_demote: 0s\n"
	assert.NoError(t, yaml.Unmarshal([]byte(src2), &cfg2))
	node2 := LocalCfg{Id: "node", Port: 1, Key: NyPrivateKey{1}}
	node2.Tunables = cfg2.Tunables
	assert.NoError(t, NodeConfigValidator(nil, &node2))
}
