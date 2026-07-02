package config

import (
	"bytes"
	"embed"
	"fmt"
	"strings"
	"text/template"
)

//go:embed templates/*.hcl
var templateFS embed.FS

// Profile names — passed via --generate-config=PROFILE.
const (
	ProfileSingle       = "single"
	ProfileHA           = "ha"
	ProfileExternalEtcd = "external-etcd"
)

func IsValidProfile(s string) bool {
	switch s {
	case ProfileSingle, ProfileHA, ProfileExternalEtcd:
		return true
	}
	return false
}

func ListProfiles() []string {
	return []string{ProfileSingle, ProfileHA, ProfileExternalEtcd}
}

// templateVars feeds the embedded HCL template.
type templateVars struct {
	ClusterName string
	ClusterCIDR string
	SVCCIDR     string
	Master1     string
	Master2     string
	Master3     string
	Worker1     string
	Worker2     string
	SSHKeyFile  string
	VIP         string
	VIPIface    string
	EtcdHosts   string // comma-separated for hcl list; e.g. `10.0.0.31","10.0.0.32","10.0.0.33`
}

// RenderTemplate returns the rendered HCL bytes for the named profile.
// names should be one of ProfileSingle / ProfileHA / ProfileExternalEtcd.
func RenderTemplate(profile string, vars templateVars) ([]byte, error) {
	if !IsValidProfile(profile) {
		return nil, fmt.Errorf("unknown profile %q (want one of: %s)",
			profile, strings.Join(ListProfiles(), ", "))
	}
	path := "templates/" + profile + ".hcl"
	body, err := templateFS.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("load template %q: %w", path, err)
	}
	tpl, err := template.New(profile).Parse(string(body))
	if err != nil {
		return nil, fmt.Errorf("parse template %q: %w", path, err)
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, vars); err != nil {
		return nil, fmt.Errorf("execute template %q: %w", path, err)
	}
	return buf.Bytes(), nil
}

// DefaultVars returns a populated templateVars with sealos-style placeholder values.
// Callers can override individual fields before calling RenderTemplate.
func DefaultVars() templateVars {
	return templateVars{
		ClusterName: "demo",
		ClusterCIDR: "10.244.0.0/16",
		SVCCIDR:     "10.96.0.0/12",
		Master1:     "10.0.0.11",
		Master2:     "10.0.0.12",
		Master3:     "10.0.0.13",
		Worker1:     "10.0.0.21",
		Worker2:     "10.0.0.22",
		SSHKeyFile:  "/root/.ssh/id_rsa",
		VIP:         "10.0.0.10",
		VIPIface:    "eth0",
		EtcdHosts:   `"10.0.0.31", "10.0.0.32", "10.0.0.33"`,
	}
}
