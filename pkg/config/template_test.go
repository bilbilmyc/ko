package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderTemplate_KnownProfiles(t *testing.T) {
	for _, p := range ListProfiles() {
		p := p
		t.Run(p, func(t *testing.T) {
			out, err := RenderTemplate(p, DefaultVars())
			require.NoError(t, err)
			require.NotEmpty(t, out)
			text := string(out)
			// Each template must include the profile marker
			assert.Contains(t, text, "profile: "+p,
				"template header should declare profile name")
			// And parse back as a valid HCL config
			path := filepath.Join(t.TempDir(), "cluster.hcl")
			require.NoError(t, os.WriteFile(path, out, 0o644))
			cfg, err := ParseFile(path)
			require.NoError(t, err)
			cfg.ApplyDefaults()
			// Sanity check: at least one master present in every profile
			assert.NotEmpty(t, cfg.Nodes.Masters)
		})
	}
}

func TestRenderTemplate_Unknown(t *testing.T) {
	_, err := RenderTemplate("does-not-exist", DefaultVars())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown profile")
}

func TestRenderTemplate_VarSubstitution(t *testing.T) {
	vars := DefaultVars()
	vars.ClusterName = "prod-east-1"
	vars.VIP = "10.20.30.40"
	out, err := RenderTemplate(ProfileHA, vars)
	require.NoError(t, err)
	text := string(out)
	// The template uses aligned `=` for readability, so check the value
	// appears in *some* `name  = "..."` form (not exact whitespace match).
	assert.Contains(t, text, `"prod-east-1"`)
	assert.Contains(t, text, `"10.20.30.40"`)
	assert.NotContains(t, text, "{{.", "all template vars should be resolved")
}

func TestRenderTemplate_HAHasMasters(t *testing.T) {
	out, err := RenderTemplate(ProfileHA, DefaultVars())
	require.NoError(t, err)
	text := string(out)
	assert.Contains(t, text, "10.0.0.11")
	assert.Contains(t, text, "10.0.0.12")
	assert.Contains(t, text, "10.0.0.13")
}

func TestRenderTemplate_ExternalEtcdMode(t *testing.T) {
	out, err := RenderTemplate(ProfileExternalEtcd, DefaultVars())
	require.NoError(t, err)
	text := string(out)
	assert.Contains(t, text, `mode      = "external"`)
	assert.Contains(t, text, "2379")
	assert.Contains(t, text, "10.0.0.31")
	assert.Contains(t, text, "10.0.0.32")
	assert.Contains(t, text, "10.0.0.33")
}

func TestRenderTemplate_Roundtrip(t *testing.T) {
	// End-to-end: profile → file → ParseFile → ApplyDefaults must yield
	// the same values templated in (so defaults don't disagree silently).
	out, err := RenderTemplate(ProfileSingle, DefaultVars())
	require.NoError(t, err)
	dir := t.TempDir()
	path := filepath.Join(dir, "cluster.hcl")
	require.NoError(t, os.WriteFile(path, out, 0o644))
	cfg, err := ParseFile(path)
	require.NoError(t, err)
	cfg.ApplyDefaults()
	assert.Equal(t, "demo", cfg.Cluster.Name)
	assert.Equal(t, "10.244.0.0/16", cfg.Cluster.CIDR)
	assert.Equal(t, "stacked", cfg.Etcd.Mode)
	assert.Equal(t, "cilium", cfg.CNI.Plugin)
	assert.Equal(t, []string{"10.0.0.11"}, cfg.Nodes.Masters)
}

func TestIsValidProfile(t *testing.T) {
	assert.True(t, IsValidProfile(ProfileSingle))
	assert.True(t, IsValidProfile(ProfileHA))
	assert.True(t, IsValidProfile(ProfileExternalEtcd))
	assert.False(t, IsValidProfile(""))
	assert.False(t, IsValidProfile("bogus"))
}

func TestListProfiles_StableOrder(t *testing.T) {
	got := ListProfiles()
	// Order matters for `ko init --generate-config --help` / docs.
	assert.Equal(t, []string{ProfileSingle, ProfileHA, ProfileExternalEtcd}, got)
}

func TestWriteAtomic(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "cluster.hcl")
	n, err := WriteAtomic(target, []byte("hello"), false)
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	b, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(b))
}

func TestWriteAtomic_RefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "cluster.hcl")
	require.NoError(t, os.WriteFile(target, []byte("original"), 0o644))
	_, err := WriteAtomic(target, []byte("new"), false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to overwrite")
	// file unchanged
	b, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "original", string(b))

	// but with overwrite=true the new content lands
	n, err := WriteAtomic(target, []byte("new"), true)
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	b, err = os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "new", string(b))
}

func TestWriteAtomic_AuditedPath(t *testing.T) {
	// WriteAtomic must create parent directories.
	target := filepath.Join(t.TempDir(), "nested/deeper/cluster.hcl")
	_, err := WriteAtomic(target, []byte("x"), true)
	require.NoError(t, err)
	b, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "x", string(b))
}
