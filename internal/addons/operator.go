package addons

import _ "embed"

// Embedded so install needs no network.
//
//go:embed manifests/helmchart-crds.yaml
var helmChartCRDsYAML []byte

// Lets only a node mark its own Node for removal by the operator.
//
//go:embed manifests/node-mark-policy.yaml
var nodeMarkPolicyYAML []byte

func RenderNodeMarkPolicy() []byte { return nodeMarkPolicyYAML }
