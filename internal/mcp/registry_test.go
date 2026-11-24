package mcp

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/registry"
	"github.com/ctrliq/stratum/internal/version"
)

func configuredSchemaTag(t *testing.T, out any) string {
	t.Helper()
	f, ok := reflect.TypeOf(out).FieldByName("Configured")
	if !ok {
		t.Fatalf("%T has no Configured field", out)
	}
	return f.Tag.Get("jsonschema")
}

func redirectRegistryPaths(t *testing.T) (certsDir, regYaml string) {
	t.Helper()
	root := t.TempDir()
	certsDir = filepath.Join(root, "certs.d")
	regYaml = filepath.Join(root, "registries.yaml")
	oc, od, or := version.CertsDir, version.ContainerdConfD, version.RegistriesConfig
	t.Cleanup(func() { version.CertsDir, version.ContainerdConfD, version.RegistriesConfig = oc, od, or })
	version.CertsDir = certsDir
	version.ContainerdConfD = filepath.Join(root, "conf.d", "stratum-registry.toml")
	version.RegistriesConfig = regYaml
	return certsDir, regYaml
}

func TestRegistryToolsConfigsOnly(t *testing.T) {
	fakeStratumHost(t)
	_, regYaml := redirectRegistryPaths(t)
	if err := os.WriteFile(regYaml, []byte("configs:\n  \"registry.corp:5000\":\n    auth: {username: u, password: p}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	_, applyOut, err := RegistryApplyHandler()(ctx, nil, RegistryApplyIn{})
	if err != nil {
		t.Fatal(err)
	}
	if !applyOut.Configured {
		t.Errorf("registry_apply on a configs-only file reported configured=false: %+v", applyOut)
	}
	if len(applyOut.Registries) != 1 || applyOut.Registries[0] != "registry.corp:5000" {
		t.Errorf("registry_apply Registries = %v, want [registry.corp:5000]", applyOut.Registries)
	}

	_, checkOut, err := RegistryCheckHandler()(ctx, nil, RegistryCheckIn{})
	if err != nil {
		t.Fatal(err)
	}
	if !checkOut.Configured {
		t.Errorf("registry_check on a configs-only file reported configured=false: %+v", checkOut)
	}
	var found bool
	for _, row := range checkOut.Configs {
		if row.Registry == "registry.corp:5000" {
			found = true
			if !row.HasAuth {
				t.Errorf("configs row for registry.corp:5000 should report credentials: %+v", row)
			}
		}
	}
	if !found {
		t.Errorf("registry_check should list registry.corp:5000 under configs, got %+v", checkOut.Configs)
	}
}

func TestRegistryToolsEmptyFileAgree(t *testing.T) {
	fakeStratumHost(t)
	_, regYaml := redirectRegistryPaths(t)
	if err := os.WriteFile(regYaml, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	_, applyOut, err := RegistryApplyHandler()(ctx, nil, RegistryApplyIn{})
	if err != nil {
		t.Fatal(err)
	}
	if applyOut.Configured {
		t.Errorf("empty file reported configured=true: %+v", applyOut)
	}
	if !strings.Contains(applyOut.Note, registry.NothingConfigured) {
		t.Errorf("apply note should carry the shared nothing-configured sentence, got %q", applyOut.Note)
	}

	_, checkOut, err := RegistryCheckHandler()(ctx, nil, RegistryCheckIn{})
	if err != nil {
		t.Fatal(err)
	}
	if checkOut.Configured {
		t.Errorf("empty file reported configured=true: %+v", checkOut)
	}
	if !strings.Contains(checkOut.Note, registry.NothingConfigured) {
		t.Errorf("check note should carry the shared nothing-configured sentence, got %q", checkOut.Note)
	}
}

func TestRegistryToolsAuthlessConfigsIsNothing(t *testing.T) {
	fakeStratumHost(t)
	_, regYaml := redirectRegistryPaths(t)
	if err := os.WriteFile(regYaml, []byte("configs:\n  \"registry.corp:5000\": {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	_, applyOut, err := RegistryApplyHandler()(ctx, nil, RegistryApplyIn{})
	if err != nil {
		t.Fatal(err)
	}
	if applyOut.Configured {
		t.Errorf("apply reported configured=true for a configs entry with neither auth nor TLS: %+v", applyOut)
	}
	_, checkOut, err := RegistryCheckHandler()(ctx, nil, RegistryCheckIn{})
	if err != nil {
		t.Fatal(err)
	}
	if checkOut.Configured {
		t.Errorf("check reported configured=true for a configs entry with neither auth nor TLS: %+v", checkOut)
	}
	if !strings.Contains(checkOut.Note, registry.NothingConfigured) {
		t.Errorf("check note should carry the shared nothing-configured sentence, got %q", checkOut.Note)
	}
}

func TestRegistryToolsWildcardCredentialsWarn(t *testing.T) {
	for _, tc := range []struct {
		name, yaml     string
		wantConfigured bool
	}{
		{"credentials and CA", "configs:\n  \"*\":\n    auth: {username: u, password: p}\n    tls: {ca_file: /etc/pki/ca.crt}\n", true},
		{"credentials only", "configs:\n  \"*\":\n    auth: {username: u, password: p}\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeStratumHost(t)
			_, regYaml := redirectRegistryPaths(t)
			if err := os.WriteFile(regYaml, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()

			_, applyOut, err := RegistryApplyHandler()(ctx, nil, RegistryApplyIn{})
			if err != nil {
				t.Fatal(err)
			}
			if applyOut.Configured != tc.wantConfigured {
				t.Errorf("registry_apply configured = %v, want %v", applyOut.Configured, tc.wantConfigured)
			}
			if !slices.Contains(applyOut.Warnings, registry.WildcardCredentialsIgnored) {
				t.Errorf("registry_apply warnings = %v, want the wildcard-credentials warning", applyOut.Warnings)
			}

			_, checkOut, err := RegistryCheckHandler()(ctx, nil, RegistryCheckIn{})
			if err != nil {
				t.Fatal(err)
			}
			if checkOut.Configured != tc.wantConfigured {
				t.Errorf("registry_check configured = %v, want %v", checkOut.Configured, tc.wantConfigured)
			}
			if !strings.Contains(checkOut.Note, registry.WildcardCredentialsIgnored) {
				t.Errorf("registry_check note = %q, want the wildcard-credentials warning", checkOut.Note)
			}
			for _, row := range checkOut.Configs {
				if row.Registry == "*" && row.HasAuth {
					t.Errorf("registry_check reports credentials it will not apply: %+v", row)
				}
			}
		})
	}
}

func TestRegistryConfiguredSchemaStrings(t *testing.T) {
	for _, row := range []struct {
		typeName string
		out      any
		want     string
	}{
		{"RegistryApplyOut", RegistryApplyOut{}, "registries.yaml has mirrors or registry credentials/TLS, and they were applied"},
		{"RegistryCheckOut", RegistryCheckOut{}, "false when registries.yaml is absent or configures nothing, which is the common case"},
	} {
		if got := configuredSchemaTag(t, row.out); got != row.want {
			t.Errorf("%s.Configured jsonschema = %q, want %q", row.typeName, got, row.want)
		}
	}
}
