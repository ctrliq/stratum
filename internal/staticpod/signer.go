package staticpod

import "github.com/ctrliq/stratum/internal/pki"

// signer names the key pair that signs for a base name; see pki.Signer. A var
// so tests can stand in a forced rotation's lost key.
var signer = pki.Signer
