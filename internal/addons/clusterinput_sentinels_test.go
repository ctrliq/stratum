package addons

import (
	"bytes"
	"testing"
)

// A regeneration that moves the replaced line would silently no-op the staging substitution.
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
