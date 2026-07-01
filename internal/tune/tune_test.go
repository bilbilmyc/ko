package tune

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestProfile_ProductionHasRequiredKeys(t *testing.T) {
	p := LookupProfile("production")
	require := assert.New(t)
	require.NotNil(p)
	require.Equal("1", p.Sysctl["net.ipv4.ip_forward"])
	require.Equal("1", p.Sysctl["net.bridge.bridge-nf-call-iptables"])
	require.Equal("0", p.Sysctl["vm.swappiness"])
	require.Contains(p.Modules, "br_netfilter")
	require.Contains(p.Modules, "overlay")
}

func TestProfile_LookupUnknown(t *testing.T) {
	assert.Nil(t, LookupProfile("not-a-profile"))
}

func TestConfig_Resolved_OverridesWin(t *testing.T) {
	c := Config{
		Profile: "production",
		Sysctl: map[string]string{
			"vm.swappiness": "10", // override production's 0
		},
	}
	p, err := c.Resolved()
	assert.NoError(t, err)
	assert.Equal(t, "10", p.Sysctl["vm.swappiness"])
	// other production keys still present
	assert.Equal(t, "1", p.Sysctl["net.ipv4.ip_forward"])
}

func TestConfig_Resolved_AddsModules(t *testing.T) {
	c := Config{
		Profile:       "production",
		KernelModules: []string{"nf_nat", "ip_tables"},
	}
	p, err := c.Resolved()
	assert.NoError(t, err)
	// production base + extra
	assert.Contains(t, p.Modules, "br_netfilter")
	assert.Contains(t, p.Modules, "nf_nat")
	assert.Contains(t, p.Modules, "ip_tables")
}