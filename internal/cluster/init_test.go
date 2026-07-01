package cluster

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ko-build/ko/pkg/config"
)

func TestInit_AllHosts_Dedupes(t *testing.T) {
	i := &Init{
		Cfg: &config.File{
			Nodes: config.NodesBlock{
				Masters: []string{"m1", "m2", "m1"}, // dup
				Workers: []string{"m1", "w1", "w2"}, // m1 already in masters
			},
		},
	}
	got := i.allHosts()
	assert.Equal(t, []string{"m1", "m2", "w1", "w2"}, got)
}

func TestInit_IsHA(t *testing.T) {
	single := &Init{Cfg: &config.File{Nodes: config.NodesBlock{Masters: []string{"m1"}}}}
	ha := &Init{Cfg: &config.File{Nodes: config.NodesBlock{Masters: []string{"m1", "m2", "m3"}}}}
	assert.False(t, single.isHA())
	assert.True(t, ha.isHA())
}

func TestInit_NeedsFlannel_DefaultCilium(t *testing.T) {
	i := &Init{Cfg: &config.File{
		CNI: config.CNIBlock{Plugin: "cilium"},
	}}
	assert.False(t, i.needsFlannel())
}

func TestInit_NeedsFlannel_DefaultFlannel(t *testing.T) {
	i := &Init{Cfg: &config.File{
		CNI: config.CNIBlock{Plugin: "flannel"},
	}}
	assert.True(t, i.needsFlannel())
}

func TestInit_NeedsFlannel_PerNodeOverride(t *testing.T) {
	i := &Init{Cfg: &config.File{
		CNI: config.CNIBlock{Plugin: "cilium"},
		NodesOverride: []config.NodesOverrideBlock{
			{Host: "old-kernel-1", CNI: "flannel"},
		},
	}}
	assert.True(t, i.needsFlannel())
}

func TestInit_Masters_ReturnsList(t *testing.T) {
	i := &Init{Cfg: &config.File{
		Nodes: config.NodesBlock{Masters: []string{"m1", "m2"}},
	}}
	assert.Equal(t, []string{"m1", "m2"}, i.masters())
}