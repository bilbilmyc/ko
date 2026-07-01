package cluster

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFlannel_Values_DefaultBackend(t *testing.T) {
	f := &FlannelInstaller{Backend: ""}
	v := f.Values("10.244.0.0/16")
	assert.Equal(t, "vxlan", v["backend"])
	assert.Equal(t, "10.244.0.0/16", v["podCidr"])
}

func TestFlannel_Values_HostGW(t *testing.T) {
	f := &FlannelInstaller{Backend: "host-gw"}
	v := f.Values("10.244.0.0/16")
	assert.Equal(t, "host-gw", v["backend"])
}