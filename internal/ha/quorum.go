package ha

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type localCluster struct {
	ClusterID uint64
	SelfID    uint64
	Members   []*etcdserverpb.Member
}

func confirmQuorumLost(list func() (localCluster, error), peerClusterID func(*etcdserverpb.Member) (uint64, bool)) error {
	lc, err := list()
	if err != nil {
		return fmt.Errorf("cannot read this node's etcd membership (%w), so it is unknown whether its cluster still has quorum", err)
	}
	voters, alive, unknown := 0, 0, 0
	for _, m := range lc.Members {
		if m.IsLearner {
			continue
		}
		voters++
		if m.ID == lc.SelfID {
			alive++
		} else if id, ok := peerClusterID(m); !ok {
			unknown++
		} else if id == lc.ClusterID {
			alive++
		}
	}
	if voters == 0 {
		return fmt.Errorf("this node's etcd reports no voting members, so it is unknown whether its cluster still has quorum")
	}
	need := voters/2 + 1
	switch {
	case alive >= need:
		return fmt.Errorf("this node's etcd cluster still has quorum: %d of its %d voting members answer, %d are needed", alive, voters, need)
	case alive+unknown >= need:
		return fmt.Errorf("cannot rule out that this node's etcd cluster still has quorum: %d of its %d voting members "+
			"answer in this cluster and %d could not be reached (they may hold a majority), %d are needed", alive, voters, unknown, need)
	}
	return nil
}

// a var so RejoinEtcd's tests can substitute the real etcd
var rejoinConfirmQuorumLost = func() error {
	return confirmQuorumLost(localMembership, peerClusterID)
}

func localMembership() (localCluster, error) {
	cli, err := etcdClient("127.0.0.1")
	if err != nil {
		return localCluster{}, err
	}
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ml, err := cli.MemberList(ctx, clientv3.WithSerializable())
	if err != nil {
		return localCluster{}, err
	}
	return localCluster{ClusterID: ml.Header.ClusterId, SelfID: ml.Header.MemberId, Members: ml.Members}, nil
}

// a peer's loopback client URL is this node's own etcd, not the peer's
func peerClusterID(m *etcdserverpb.Member) (uint64, bool) {
	tc, err := etcdTLS()
	if err != nil {
		return 0, false
	}
	for _, cu := range m.ClientURLs {
		if u, err := url.Parse(cu); err != nil || u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" {
			continue
		}
		cli, err := clientv3.New(clientv3.Config{Endpoints: []string{cu}, TLS: tc, DialTimeout: 3 * time.Second})
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		st, err := cli.Status(ctx, cu)
		cancel()
		cli.Close()
		if err == nil {
			return st.Header.ClusterId, true
		}
	}
	return 0, false
}
