package cluster

import (
	"github.com/ctrliq/stratum/internal/ha"
)

// assembleNode is the shared node assembly the controller runs — ha.AssembleNodeContext,
// the ONE assembly the HA join's preparation runs too — behind a seam so a test
// can stand in for it and observe that PrepareController routes its common node
// steps (host directories, prerequisites, pod-CIDR trust, node-IP resolution,
// PKI, kubeconfigs, CIS configs, runtime configs) through it instead of keeping
// a second inline copy of the sequence: a change to one path's node rendering
// must reach the other.
var assembleNode = ha.AssembleNodeContext
