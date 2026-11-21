package cli

import (
	"context"

	"k8s.io/client-go/kubernetes"

	"github.com/ctrliq/stratum/internal/trustaudit"
)

type AuditResult int

const (
	AuditPass AuditResult = iota + 1
	// AuditFail also covers a check that could not read what it must.
	AuditFail
	// AuditException reports the property as unverified, not violated.
	AuditException
)

func (r AuditResult) String() string {
	switch r {
	case AuditPass:
		return "PASS"
	case AuditFail:
		return "FAIL"
	case AuditException:
		return "EXCEPTION"
	}
	return "unknown"
}

type AuditVerdict struct {
	Result AuditResult
	Detail string
}

func auditVerdict(v trustaudit.Verdict) AuditVerdict {
	r := AuditException
	switch v.Result {
	case trustaudit.Pass:
		r = AuditPass
	case trustaudit.Fail:
		r = AuditFail
	}
	return AuditVerdict{Result: r, Detail: v.Detail}
}

// Reports whether the node's running etcd refuses a root-signed client certificate.
func AuditEtcdTrust(ctx context.Context) AuditVerdict {
	return auditVerdict(trustaudit.EtcdTrust(ctx))
}

// Reports whether this node's kubelet trusts a client CA distinct from the cluster root.
func AuditKubeletClientTrust() AuditVerdict {
	return auditVerdict(trustaudit.KubeletClientTrust())
}

// Reports whether every kubelet refuses a root-signed client certificate.
func AuditKubeletsRefuseRoot(ctx context.Context, cs kubernetes.Interface) AuditVerdict {
	return auditVerdict(trustaudit.KubeletsRefuseRoot(ctx, cs))
}
