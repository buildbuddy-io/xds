package snapshot

import (
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
)

func TestClaWithPrioritiesZone(t *testing.T) {
	cla := &endpointv3.ClusterLoadAssignment{
		ClusterName: "svc",
		Endpoints: []*endpointv3.LocalityLbEndpoints{
			{Locality: &corev3.Locality{Zone: "zone-a"}},
			{Locality: &corev3.Locality{Zone: "zone-b"}},
			{Locality: &corev3.Locality{Zone: "zone-c"}},
		},
	}

	out := assignmentWithPriorities(cla, "zone-b", "")

	if got := len(out.Endpoints); got != 3 {
		t.Fatalf("expected 3 groups, got %d", got)
	}
	// zone-b should be priority 0; everyone else priority 1, and must be
	// contiguous.
	seenPri := map[uint32]string{}
	for _, g := range out.Endpoints {
		seenPri[g.Priority] = g.GetLocality().GetZone()
	}
	if seenPri[0] != "zone-b" {
		t.Errorf("priority 0 = %q, want zone-b", seenPri[0])
	}
	foundP1 := 0
	for _, g := range out.Endpoints {
		if g.Priority == 1 {
			foundP1++
		}
	}
	if foundP1 != 2 {
		t.Errorf("priority 1 count = %d, want 2", foundP1)
	}
	// Must be contiguous. No priority 2.
	for _, g := range out.Endpoints {
		if g.Priority > 1 {
			t.Errorf("unexpected priority %d (priorities must be contiguous)", g.Priority)
		}
	}
}

func TestClaWithPrioritiesSubZone(t *testing.T) {
	cla := &endpointv3.ClusterLoadAssignment{
		ClusterName: "svc",
		Endpoints: []*endpointv3.LocalityLbEndpoints{
			{Locality: &corev3.Locality{Zone: "z1", SubZone: "r1"}}, // exact
			{Locality: &corev3.Locality{Zone: "z1", SubZone: "r2"}}, // same zone
			{Locality: &corev3.Locality{Zone: "z2", SubZone: "r1"}}, // diff zone
		},
	}

	out := assignmentWithPriorities(cla, "z1", "r1")

	pri := map[string]uint32{}
	for _, g := range out.Endpoints {
		loc := g.GetLocality()
		pri[loc.Zone+"/"+loc.SubZone] = g.Priority
	}
	if pri["z1/r1"] != 0 {
		t.Errorf("exact match priority = %d, want 0", pri["z1/r1"])
	}
	if pri["z1/r2"] != 1 {
		t.Errorf("same-zone priority = %d, want 1", pri["z1/r2"])
	}
	if pri["z2/r1"] != 2 {
		t.Errorf("diff-zone priority = %d, want 2", pri["z2/r1"])
	}
}

func TestClaWithPrioritiesCompaction(t *testing.T) {
	// Only same-zone and diff-zone exist (no exact sub_zone match). The
	// output must start at priority 0 and stay contiguous.
	cla := &endpointv3.ClusterLoadAssignment{
		Endpoints: []*endpointv3.LocalityLbEndpoints{
			{Locality: &corev3.Locality{Zone: "z1", SubZone: "r2"}},
			{Locality: &corev3.Locality{Zone: "z2", SubZone: "r1"}},
		},
	}

	out := assignmentWithPriorities(cla, "z1", "r1")

	maxPri := uint32(0)
	sawZero := false
	for _, g := range out.Endpoints {
		if g.Priority == 0 {
			sawZero = true
		}
		if g.Priority > maxPri {
			maxPri = g.Priority
		}
	}
	if !sawZero {
		t.Error("missing priority 0")
	}
	if maxPri != 1 {
		t.Errorf("max priority = %d, want 1 (contiguous)", maxPri)
	}
}

func TestClaWithPrioritiesNoClientLocality(t *testing.T) {
	// Client has no locality info. All groups get the same priority (0)
	// so no preference is expressed.
	cla := &endpointv3.ClusterLoadAssignment{
		Endpoints: []*endpointv3.LocalityLbEndpoints{
			{Locality: &corev3.Locality{Zone: "z1"}},
			{Locality: &corev3.Locality{Zone: "z2"}},
		},
	}

	out := assignmentWithPriorities(cla, "", "")

	for _, g := range out.Endpoints {
		if g.Priority != 0 {
			t.Errorf("got priority %d with empty client locality; all should be 0", g.Priority)
		}
	}
}

func TestResourcesForLocalityPassesThroughNonEndpoints(t *testing.T) {
	in := map[string][]types.Resource{
		resource.ClusterType: {&endpointv3.ClusterLoadAssignment{ClusterName: "x"}}, // wrong type on purpose
	}
	out := resourcesForLocality("z", "", in)
	if _, ok := out[resource.EndpointType]; ok {
		t.Error("unexpected endpoint resources added")
	}
	if len(out[resource.ClusterType]) != 1 {
		t.Error("non-endpoint resources should pass through unchanged")
	}
}

func TestLocalityNodeHashIncludesSubZone(t *testing.T) {
	h := localityNodeHash{}
	a := h.ID(&corev3.Node{Locality: &corev3.Locality{Zone: "z1", SubZone: "r1"}})
	b := h.ID(&corev3.Node{Locality: &corev3.Locality{Zone: "z1", SubZone: "r2"}})
	if a == b {
		t.Errorf("expected distinct hashes for different sub_zone; both are %q", a)
	}
}
