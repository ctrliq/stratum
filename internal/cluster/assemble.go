package cluster

import (
	"github.com/ctrliq/stratum/internal/ha"
)

// A var so a test can stub the shared node assembly.
var assembleNode = ha.AssembleNodeContext
