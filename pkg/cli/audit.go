package cli

import (
	"context"

	"k8s.io/client-go/kubernetes"

	"github.com/ctrliq/stratum/internal/trustaudit"
)

// AuditResult is the outcome of one engine audit.
type AuditResult int

const (
	// AuditPass means the audit verified the property.
	AuditPass AuditResult = iota + 1
	// AuditFail means the audit found the property violated, or could not read what it must.
	AuditFail
	// AuditException means the audit could not verify the property and reports it unverified.
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

// AuditVerdict is an audit's result and the detail an operator reads.
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

// AuditEtcdTrust reports whether this node's running etcd refuses a client certificate the cluster root signs.
func AuditEtcdTrust(ctx context.Context) AuditVerdict {
	return auditVerdict(trustaudit.EtcdTrust(ctx))
}

// AuditKubeletClientTrust reports whether this node's kubelet trusts a client CA distinct from the cluster root.
func AuditKubeletClientTrust() AuditVerdict {
	return auditVerdict(trustaudit.KubeletClientTrust())
}

// AuditKubeletsRefuseRoot reports whether every kubelet cs lists refuses a root-signed client certificate and accepts apiserver-kubelet-client.
func AuditKubeletsRefuseRoot(ctx context.Context, cs kubernetes.Interface) AuditVerdict {
	return auditVerdict(trustaudit.KubeletsRefuseRoot(ctx, cs))
}
