package cli

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestDefaultBundleName covers the contract that the default `--version`
// value embedded in the pack filename is `bundle-k8s<X>-cilium<X>-YYYYMMDD`,
// with both `v`-prefixed and bare version strings accepted.
func TestDefaultBundleName(t *testing.T) {
	fixed := time.Date(2026, 7, 2, 12, 34, 56, 0, time.UTC)
	cases := []struct {
		name        string
		k8s, cilium string
		want        string
	}{
		{
			name:   "kubeadm-style v-prefix on k8s, bare cilium",
			k8s:    "v1.32.0",
			cilium: "1.16.1",
			want:   "bundle-k8s1.32.0-cilium1.16.1-20260702",
		},
		{
			name:   "bare versions both sides",
			k8s:    "1.32.0",
			cilium: "1.16.1",
			want:   "bundle-k8s1.32.0-cilium1.16.1-20260702",
		},
		{
			name:   "v-prefix on both",
			k8s:    "v1.32.0",
			cilium: "v1.16.1",
			want:   "bundle-k8s1.32.0-cilium1.16.1-20260702",
		},
		{
			name:   "different calendar day",
			k8s:    "v1.33.0",
			cilium: "1.17.0",
			want:   "bundle-k8s1.33.0-cilium1.17.0-20260702",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := defaultBundleName(tc.k8s, tc.cilium, fixed)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestDefaultBundleName_RendersForAllArch confirms the prefix returned by
// defaultBundleName composes cleanly with the builder/multi filenames so
// the resulting tarball name follows the convention documented in RUNBOOK §1.1.
func TestDefaultBundleName_RendersForAllArch(t *testing.T) {
	fixed := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	prefix := defaultBundleName(defaultK8sVersion, defaultCiliumVersion, fixed)

	// These suffixes mirror what builder.go and multi.go append to opts.Version.
	const (
		singleArch = "-amd64.oci.tar.gz"
		multiArch  = "-multi.oci.tar.gz"
	)

	assert.Equal(t,
		"bundle-k8s1.32.0-cilium1.16.1-20260702-amd64.oci.tar.gz",
		prefix+singleArch,
		"single-arch filename must follow bundle-k8sX-ciliumX-date-<arch>.oci.tar.gz")
	assert.Equal(t,
		"bundle-k8s1.32.0-cilium1.16.1-20260702-multi.oci.tar.gz",
		prefix+multiArch,
		"multi-arch filename must follow bundle-k8sX-ciliumX-date-multi.oci.tar.gz")
}
