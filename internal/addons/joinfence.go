package addons

import _ "embed"

// The admission policy that holds a control-plane join token to its own
// maintenance fence. It is not one of the stacks the manifest deployer
// applies: the mint that grants join tokens the maintenance leases puts it in
// place first (ha.GrantCertsReader), via RenderJoinFencePolicy.
//
//go:embed manifests/join-fence-policy.yaml
var joinFencePolicyYAML []byte

// RenderJoinFencePolicy is the join fence admission policy and its binding.
func RenderJoinFencePolicy() []byte { return joinFencePolicyYAML }
