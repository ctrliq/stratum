package addons

import (
	"bytes"
	"testing"
)

// The staged cilium and local-path renders leave their one per-cluster input as
// a sentinel the deployer fills at apply time, staged by a bytes.ReplaceAll of
// an exact upstream line. If a regeneration of the upstream manifest moves,
// re-quotes or rewrites that line, the substitution silently no-ops: the staged
// bundle carries the staging node's own answer for every control plane, and a
// dual-stack cluster staged by a single-stack node deploys IPv4-only Cilium, or
// local-path claims the default StorageClass whatever the config says. These
// guards pin each sentinel to exactly one occurrence and its upstream line to
// none, so a regeneration that breaks the substitution fails here instead of
// deploying quietly — the same count-guard
// TestCoreDNSServiceIPIsStagedAsASentinel holds the CoreDNS sentinels to.
func TestCiliumStagedCarriesTheDualStackSentinelOnce(t *testing.T) {
	m := RenderCiliumStaged(CiliumSettings{})
	if got := bytes.Count(m, []byte(`enable-ipv6: "`+EnableIPv6Sentinel+`"`)); got != 1 {
		t.Errorf("the staged cilium render carries the enable-ipv6 sentinel %d times, want exactly once", got)
	}
	if got := bytes.Count(m, []byte(`enable-ipv6: "false"`)); got != 0 {
		t.Errorf(`the staged cilium render still carries the literal enable-ipv6 "false" %d times; the staging must replace it with the sentinel`, got)
	}
}

func TestLocalPathStagedCarriesTheStorageDefaultSentinelOnce(t *testing.T) {
	m := RenderLocalPath()
	if got := bytes.Count(m, []byte(`storageclass.kubernetes.io/is-default-class: "`+StorageDefaultSentinel+`"`)); got != 1 {
		t.Errorf("the staged local-path render carries the storage-default sentinel %d times, want exactly once", got)
	}
	if got := bytes.Count(m, []byte(`storageclass.kubernetes.io/is-default-class: "true"`)); got != 0 {
		t.Errorf(`the staged local-path render still carries the literal is-default-class "true" %d times; the staging must replace it with the sentinel`, got)
	}
}
