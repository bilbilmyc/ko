package cluster

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestKubeVip_Values_HA(t *testing.T) {
	k := &KubeVipInstaller{
		Image: "ghcr.io/kube-vip/kube-vip:v0.6.4",
		VIP:   "10.0.0.100",
	}
	v := k.Values("eth0", "10.0.0.100")
	assert.Equal(t, "ghcr.io/kube-vip/kube-vip:v0.6.4", v["image"])
	assert.Equal(t, "eth0", v["interface"])
	assert.Equal(t, "eth0", v["vip_interface"])
	assert.Equal(t, true, v["controlPlane"])
	assert.Equal(t, "10.0.0.100", v["address"])
	assert.Equal(t, true, v["servicesEnabled"])
}

func TestKubeVip_Values_DefaultImage(t *testing.T) {
	k := &KubeVipInstaller{VIP: "10.0.0.100"}
	v := k.Values("eth0", "10.0.0.100")
	assert.Equal(t, "ghcr.io/kube-vip/kube-vip:latest", v["image"])
}