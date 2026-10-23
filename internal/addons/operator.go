package addons

import _ "embed"

// HelmChart / HelmChartConfig CRDs the operator reconciles. Embedded so install
// needs no network access; shipped to the cluster by the manifest deployer via
// RenderHelmCRDs (internal/cluster.writeBundles).
//
//go:embed manifests/helmchart-crds.yaml
var helmChartCRDsYAML []byte

// The admission policy that lets only a node mark its own Node for removal
// by the operator: shipped by the manifest deployer beside the
// operator's CRDs, via RenderNodeMarkPolicy.
//
//go:embed manifests/node-mark-policy.yaml
var nodeMarkPolicyYAML []byte

// RenderNodeMarkPolicy is the node-mark admission policy stack.
func RenderNodeMarkPolicy() []byte { return nodeMarkPolicyYAML }
