package cluster

import (
	"github.com/ctrliq/stratum/internal/ha"
)

// the HA join's node assembly too: a change to one path must reach the other; a var for test stubs
var assembleNode = ha.AssembleNodeContext
