package doctor

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseKernelMajorMinor(t *testing.T) {
	cases := []struct {
		in                    string
		major, minor          int
		ok                    bool
	}{
		{"5.15.0-91-generic", 5, 15, true},
		{"6.1.0", 6, 1, true},
		{"5.4.0", 5, 4, true},
		{"4.19.0", 4, 19, true},
		{"garbage", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, tc := range cases {
		ma, mi, ok := parseKernelMajorMinor(tc.in)
		assert.Equal(t, tc.ok, ok, tc.in)
		assert.Equal(t, tc.major, ma, tc.in)
		assert.Equal(t, tc.minor, mi, tc.in)
	}
}

func TestKernelSatisfies(t *testing.T) {
	assert.True(t, kernelOK("5.15.0-91"))
	assert.True(t, kernelOK("6.1.0"))
	assert.False(t, kernelOK("4.19.0"))
	assert.False(t, kernelOK(""))
}

// kernelOK mirrors the heuristic in checkKernelRemote.
func kernelOK(ver string) bool {
	ma, mi, ok := parseKernelMajorMinor(ver)
	if !ok {
		return false
	}
	return ma > 5 || (ma == 5 && mi >= 4)
}