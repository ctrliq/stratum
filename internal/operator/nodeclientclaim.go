package operator

import (
	"context"
	"crypto/sha256"
	"fmt"

	certv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const nodeClientClaimLabel = "stratum.ciq.com/node-client-claim"

// a certificate remains usable after its CSR is garbage-collected; keep the claim independently.
func nodeClientClaimName(nodeName string) string {
	return fmt.Sprintf("stratum-node-client-claim-%x", sha256.Sum256([]byte(nodeName)))
}

// its name is held like any approved one, so deleting the Node cannot hand it to a new join.
func registeredNodeOwner(n *corev1.Node) string { return "node/" + string(n.UID) }

func nodeClientClaims(ctx context.Context, cs kubernetes.Interface) (map[string]string, error) {
	list, err := cs.CoreV1().ConfigMaps(metav1.NamespaceSystem).List(ctx, metav1.ListOptions{LabelSelector: nodeClientClaimLabel + "=true"})
	if err != nil {
		return nil, err
	}
	owners := make(map[string]string, len(list.Items))
	for _, cm := range list.Items {
		owners[cm.Data["nodeName"]] = cm.Data["csrUID"]
	}
	return owners, nil
}

// the claim is retained after the CSR or Node is removed: neither revokes an issued certificate.
func reserveNodeClientName(ctx context.Context, cs kubernetes.Interface, csr *certv1.CertificateSigningRequest, nodeName string) (bool, error) {
	if csr.UID == "" {
		return false, fmt.Errorf("CSR %s has no UID", csr.Name)
	}
	return reserveNodeClaim(ctx, cs, nodeName, string(csr.UID), csr.Name)
}

func reserveNodeClaim(ctx context.Context, cs kubernetes.Interface, nodeName, owner, ownerName string) (bool, error) {
	client := cs.CoreV1().ConfigMaps(metav1.NamespaceSystem)
	name := nodeClientClaimName(nodeName)
	claim, err := client.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		claim, err = client.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: metav1.NamespaceSystem,
				Labels: map[string]string{nodeClientClaimLabel: "true"},
			},
			Data: map[string]string{
				"nodeName": nodeName,
				"csrName":  ownerName,
				"csrUID":   owner,
			},
		}, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			claim, err = client.Get(ctx, name, metav1.GetOptions{})
		}
	}
	if err != nil {
		return false, err
	}
	if claim.Data["nodeName"] != nodeName {
		return false, fmt.Errorf("node-client claim %s has unexpected node name %q", name, claim.Data["nodeName"])
	}
	return claim.Data["csrUID"] == owner, nil
}
