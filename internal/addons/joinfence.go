package addons

import _ "embed"

// Not applied by the manifest deployer; ha.GrantCertsReader installs it first.
//
//go:embed manifests/join-fence-policy.yaml
var joinFencePolicyYAML []byte

func RenderJoinFencePolicy() []byte { return joinFencePolicyYAML }
