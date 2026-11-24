package staticpod

import "github.com/ctrliq/stratum/internal/pki"

// a var so tests can stand in a forced rotation's lost key.
var signer = pki.Signer
