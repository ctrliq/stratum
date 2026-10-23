package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/trustaudit"
)

func TestAuditResultString(t *testing.T) {
	for r, want := range map[AuditResult]string{
		AuditPass:      "PASS",
		AuditFail:      "FAIL",
		AuditException: "EXCEPTION",
		AuditResult(0): "unknown",
	} {
		if got := r.String(); got != want {
			t.Errorf("AuditResult(%d).String() = %q, want %q", int(r), got, want)
		}
	}
}

func TestAuditVerdictCarriesTheEngineResult(t *testing.T) {
	for in, want := range map[trustaudit.Result]AuditResult{
		trustaudit.Pass:      AuditPass,
		trustaudit.Fail:      AuditFail,
		trustaudit.Exception: AuditException,
	} {
		got := auditVerdict(trustaudit.Verdict{Result: in, Detail: "detail"})
		if got.Result != want || got.Detail != "detail" {
			t.Errorf("auditVerdict(%d) = %+v, want %s with the detail kept", in, got, want)
		}
	}
	if got := auditVerdict(trustaudit.Verdict{}); got.Result != AuditException {
		t.Errorf("an unset engine result maps to %s, want EXCEPTION: an audit that reports nothing is unverified, not passed", got.Result)
	}
}

func TestAuditEtcdTrustOnACancelledContextIsUnverified(t *testing.T) {
	tempLayout(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := AuditEtcdTrust(ctx); got.Result != AuditException || !strings.HasPrefix(got.Detail, "unverified: ") {
		t.Errorf("AuditEtcdTrust on a cancelled context = %s (%s), want an unverified EXCEPTION", got.Result, got.Detail)
	}
}

func TestAuditKubeletClientTrustReadsTheRenderedConfig(t *testing.T) {
	tempLayout(t)
	if got := AuditKubeletClientTrust(); got.Result != AuditException || !strings.Contains(got.Detail, "kubelet.conf not found") {
		t.Errorf("with no kubelet.conf: %s (%s), want EXCEPTION", got.Result, got.Detail)
	}
	_, _ = renderControlPlane(t, renderProfile())
	if got := AuditKubeletClientTrust(); got.Result != AuditFail || !strings.Contains(got.Detail, pki.KubeletMigrateCommand) {
		t.Errorf("on a node whose kubelet trusts the cluster root: %s (%s), want FAIL naming %s", got.Result, got.Detail, pki.KubeletMigrateCommand)
	}
}
