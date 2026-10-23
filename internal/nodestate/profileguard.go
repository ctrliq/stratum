package nodestate

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/ctrliq/stratum/internal/profile"
)

// RequireRegisteredProfile refuses op on a node whose saved profile this
// binary does not register, before op changes anything. The renderers an
// operation reaches later stop on the same profile, but mid-run, after its
// writes. op names the operation the way its command is spelled.
func (s *NodeState) RequireRegisteredProfile(op string) error {
	if _, err := profile.Select(s.ProfileName()); err != nil {
		return fmt.Errorf("%s: this node runs profile %q, which this binary does not register; nothing was changed: %w",
			op, s.ProfileName(), err)
	}
	return nil
}

// RequireNodeProfile is RequireRegisteredProfile against the saved node
// state. A node with no state yet runs no profile.
func RequireNodeProfile(op string) error {
	st, err := LoadState()
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: read the node state for its profile: %w", op, err)
	}
	return st.RequireRegisteredProfile(op)
}
