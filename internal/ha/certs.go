// Package ha distributes shared control-plane certificates for HA joins.
package ha

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/util/retry"

	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/version"
)

const certsSecret = "stratum-certs"

var encryptionConfigEntry = filepath.Base(version.EncryptionConfigPath)

// The Secret's CreationTimestamp does not move on an in-place re-key.
const certKeySetAnnotation = "stratum.ciq.com/certificate-key-set-at"

// The uploader cannot decrypt grants under earlier keys to count them.
const bundleGenerationAnnotation = "stratum.ciq.com/bundle-generation"

func nextBundleGeneration(sec *corev1.Secret) uint64 {
	n, err := strconv.ParseUint(sec.Annotations[bundleGenerationAnnotation], 10, 64)
	if err != nil || n == 0 {
		return 1
	}
	return n + 1
}

func recordedVIP() string {
	st, err := nodestate.LoadState()
	if err != nil {
		return ""
	}
	return st.VIP.Address
}

var recordedProfile = func() *string {
	st, err := nodestate.LoadState()
	if err != nil {
		return nil
	}
	name := st.ProfileName()
	return &name
}

// The proof authenticates the key itself, not a message.
const encryptionKeyProofPlaintext = "stratum: this bundle carries the at-rest encryption key"

func sealAtRestKeyProof(raw []byte) ([]byte, error) {
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(encryptionKeyProofPlaintext), nil), nil
}

// A joiner dials these for MemberAdd; etcd never listens on the --join URL's VIP.
const etcdEndpointsAnnotation = "stratum.ciq.com/etcd-client-endpoints"

// Older joiners refuse entries they do not know; these names are a cross-version contract.
func pkiTrustNames(class pki.TrustFileClass) []string {
	var out []string
	for _, f := range pki.TrustLayout() {
		if f.Class == class {
			out = append(out, f.Name)
		}
	}
	return out
}

// Byte-identical everywhere; a joining CP without the etcd CA mints a divergent anchor.
var sharedCerts = pkiTrustNames(pki.TrustShared)

// Optional, so a joiner mid-rotation receives the same in-flight trust state.
var rotationCerts = append(pkiTrustNames(pki.TrustRotation),
	netconfigEntry,
	ciliumSettingsEntry,
)

var kubeletClientEntries = map[string]bool{
	pki.KubeletClientBundleBase + "-bundle.crt":           true,
	pki.KubeletClientCAName + ".crt":                      true,
	pki.KubeletClientCAName + ".key":                      true,
	pki.StagedTrustName(pki.KubeletClientCAName + ".crt"): false,
	pki.StagedTrustName(pki.KubeletClientCAName + ".key"): false,
}

var certAllowlist = newCertAllowlist()

func newCertAllowlist() map[string]struct{} {
	m := make(map[string]struct{}, len(sharedCerts)+len(rotationCerts))
	for _, n := range sharedCerts {
		m[n] = struct{}{}
	}
	for _, n := range rotationCerts {
		m[n] = struct{}{}
	}
	return m
}

// Every profile secret must be every control plane's byte-identical copy.
func profileSecretEntries() (map[string]bool, error) {
	out := map[string]bool{}
	for _, name := range profile.SharedSecretNames() {
		if _, ok := certAllowlist[name]; ok {
			return nil, fmt.Errorf("shared certs: a registered profile's shared secret %q has the name of a file the engine shares itself", name)
		}
		out[name] = true
	}
	return out, nil
}

func carriedSecrets(files map[string][]byte, secrets map[string]bool) []string {
	var out []string
	for name := range files {
		if secrets[name] {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// Entry names become PKI filenames; an unvalidated name could write anywhere as root.
func checkCertNames(dir string, files map[string][]byte) error {
	secrets, err := profileSecretEntries()
	if err != nil {
		return err
	}
	var bad []string
	for name := range files {
		_, ok := certAllowlist[name]
		ok = ok || secrets[name]
		if !ok || filepath.Base(name) != name {
			bad = append(bad, name)
		}
	}
	if len(bad) > 0 {
		slices.Sort(bad) // deterministic message regardless of map order
		return fmt.Errorf("shared certs: refusing bundle with unexpected entries %q: "+
			"only the certs this cluster uploads are written, and never outside %s",
			bad, dir)
	}
	return nil
}

func writeCertFiles(dir string, files map[string][]byte) error {
	if err := checkCertNames(dir, files); err != nil {
		return err
	}
	secrets, err := profileSecretEntries()
	if err != nil {
		return err
	}
	if err := requireEtcdCA(files); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	names := slices.Sorted(maps.Keys(files))
	for i, name := range names {
		b := files[name]
		mode := os.FileMode(0o644)
		// A 0644 encryption config is itself a CIS finding.
		if filepath.Ext(name) == ".key" || secrets[name] {
			mode = 0o600
		}
		if err := writeCertFile(filepath.Join(dir, name), b, mode); err != nil {
			replaced := names[:i]
			if errors.Is(err, host.ErrNotDurable) {
				replaced = names[:i+1]
			}
			switch len(replaced) {
			case 0:
				return fmt.Errorf("shared certs: write %s into %s: %w; no entry was replaced", name, dir, err)
			case len(names):
				return fmt.Errorf("shared certs: write %s into %s: %w; every entry was replaced, but a power loss "+
					"could still bring back the previous %s", name, dir, err, name)
			}
			return fmt.Errorf("shared certs: write %s into %s: %w; %d of %d entries were replaced (%s), so %w",
				name, dir, err, len(replaced), len(names), strings.Join(replaced, ", "), errPartBundle)
		}
	}
	return nil
}

var errPartBundle = errors.New("this node holds part of the bundle")

var writeCertFile = host.WriteFileAtomically

func NewCertKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate certificate-key: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func aesgcm(keyHex string) (cipher.AEAD, error) {
	key, err := hex.DecodeString(keyHex)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("certificate-key must be 64 hex chars (AES-256)")
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(blk)
}

func encrypt(keyHex string, plaintext []byte) ([]byte, error) {
	gcm, err := aesgcm(keyHex)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func decrypt(keyHex string, ct []byte) ([]byte, error) {
	gcm, err := aesgcm(keyHex)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(ct) < ns {
		return nil, fmt.Errorf("ciphertext too short")
	}
	return gcm.Open(nil, ct[:ns], ct[ns:], nil)
}

const grantEntryPrefix = "certs-"

func grantEntryKey(tokenID string) string { return grantEntryPrefix + tokenID }

const tokenSecretPrefix = "bootstrap-token-"

// A token read that fails keeps its grant: an API hiccup must not drop a live grant.
func expiredGrantEntries(ctx context.Context, secrets corev1client.SecretInterface, data map[string][]byte) (expired []string, errs error) {
	// Sorted, so the written shape and any error are the same on every run.
	for _, entry := range slices.Sorted(maps.Keys(data)) {
		tokenID, ok := strings.CutPrefix(entry, grantEntryPrefix)
		if !ok || tokenID == "" {
			continue
		}
		_, err := secrets.Get(ctx, tokenSecretPrefix+tokenID, metav1.GetOptions{})
		switch {
		case err == nil:
		case apierrors.IsNotFound(err):
			expired = append(expired, entry)
		default:
			errs = errors.Join(errs, err)
		}
	}
	return expired, errs
}

// Writes nothing when nothing expired: a rewrite would bump resourceVersion unchanged.
func PruneExpiredGrants(ctx context.Context, cs kubernetes.Interface) error {
	secrets := cs.CoreV1().Secrets("kube-system")
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		sec, err := secrets.Get(ctx, certsSecret, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		expired, errs := expiredGrantEntries(ctx, secrets, sec.Data)
		if len(expired) == 0 {
			return errs
		}
		cur := sec.DeepCopy()
		for _, entry := range expired {
			delete(cur.Data, entry)
		}
		if _, err := secrets.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
			return err
		}
		return errs
	})
	if err != nil {
		return fmt.Errorf("prune kube-system/%s: %w", certsSecret, err)
	}
	return nil
}

// 64 KiB under the apiserver limit, so a too-large mint is refused here, not by it.
const grantSecretBudget = corev1.MaxSecretSize - 64<<10

func secretDataSize(data map[string][]byte) int {
	n := 0
	for name, v := range data {
		n += len(name) + len(v)
	}
	return n
}

func overfullGrantsError(data map[string][]byte, own string, unread error) error {
	var ids []string
	for _, name := range slices.Sorted(maps.Keys(data)) {
		if id, ok := strings.CutPrefix(name, grantEntryPrefix); ok && id != "" && name != own {
			ids = append(ids, id)
		}
	}
	const shown = 10
	named := strings.Join(ids, ", ")
	if len(ids) > shown {
		named = fmt.Sprintf("%s and %d more", strings.Join(ids[:shown], ", "), len(ids)-shown)
	}
	msg := fmt.Sprintf("kube-system/%s cannot hold another control-plane join grant: it already carries %d control-plane join grants "+
		"whose bootstrap tokens are still live (token IDs %s), each a full copy of the shared certificates, and this one would bring its "+
		"data to %d bytes, over the %d a mint allows under the apiserver's %d-byte Secret limit. `stratum token list` shows each token's "+
		"lifetime; delete the ones among them no longer needed (`kubectl -n kube-system delete secret bootstrap-token-<id>`), and mint "+
		"again; `stratum certs rotate --rekey-shared --yes` instead retires every pending control-plane join command at once",
		certsSecret, len(ids), named, secretDataSize(data), grantSecretBudget, corev1.MaxSecretSize)
	if unread != nil {
		msg += fmt.Sprintf(". Some grants were kept only because their bootstrap token could not be read: %v", unread)
	}
	return errors.New(msg)
}

const bundleEnvelopeVersion = 1

// Travels inside the ciphertext, so every fact is authenticated by the AES-GCM open.
type bundleMetadata struct {
	Generation       uint64   `json:"generation"`
	EtcdEndpoints    []string `json:"etcdEndpoints"`
	CarriesAtRestKey bool     `json:"carriesAtRestKey"`
	VIP              string   `json:"vip,omitempty"`
	// nil when the publisher's node state could not be read; "" when it names no profile.
	Profile *string `json:"profile,omitempty"`
}

type bundleEnvelopeV1 struct {
	Version  int               `json:"version"`
	Metadata bundleMetadata    `json:"metadata"`
	Files    map[string][]byte `json:"files"`
}

func decodeGrantPayload(plaintext []byte) (files map[string][]byte, md bundleMetadata, enveloped bool, err error) {
	var probe struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(plaintext, &probe); err != nil {
		return nil, md, false, fmt.Errorf("the bundle payload is neither the v1 envelope nor the legacy flat map: %w", err)
	}
	switch probe.Version {
	case 0:
		var files map[string][]byte
		if err := json.Unmarshal(plaintext, &files); err != nil {
			return nil, md, false, fmt.Errorf("the bundle payload is not the legacy flat map: %w", err)
		}
		return files, bundleMetadata{}, false, nil
	case bundleEnvelopeVersion:
		var env bundleEnvelopeV1
		if err := json.Unmarshal(plaintext, &env); err != nil {
			return nil, md, true, fmt.Errorf("the bundle payload names envelope version %d but is not one: %w", probe.Version, err)
		}
		if env.Files == nil {
			return nil, md, true, fmt.Errorf("the v1 envelope carries no file set")
		}
		return env.Files, env.Metadata, true, nil
	default:
		return nil, md, false, fmt.Errorf("the bundle carries envelope version %d, which this build does not support: "+
			"join with the stratum release the cluster's control planes run", probe.Version)
	}
}

type bundleGrant struct {
	Published  []string
	CarriedKey bool
	VIP        string
	Generation uint64
	bundleProfile
	// Called under the maintenance fence before a member is admitted.
	confirmCurrent func(context.Context) error
}

// bundleProfile is what a downloaded bundle says about the cluster's profile.
type bundleProfile struct {
	// A fact about the download, not the disk, where an earlier failed join may leave one.
	Secrets      []string
	Profile      string
	ProfileKnown bool
}

func openGrant(sec *corev1.Secret, plaintext []byte, check func(bundleProfile) error) (bundleGrant, error) {
	files, md, enveloped, err := decodeGrantPayload(plaintext)
	if err != nil {
		return bundleGrant{}, err
	}
	secrets, err := profileSecretEntries()
	if err != nil {
		return bundleGrant{}, err
	}
	bp := bundleProfile{Secrets: carriedSecrets(files, secrets)}
	if enveloped && md.Profile != nil {
		bp.Profile, bp.ProfileKnown = *md.Profile, true
	}
	if check != nil {
		if err := check(bp); err != nil {
			return bundleGrant{}, err
		}
	}
	carriedKey, err := applyDownloadedBundle(files)
	if err != nil {
		return bundleGrant{}, err
	}
	g := bundleGrant{
		Published:     parseEtcdEndpoints(sec.Annotations[etcdEndpointsAnnotation]),
		CarriedKey:    carriedKey,
		bundleProfile: bp,
	}
	if enveloped {
		g.Published = parseEtcdEndpoints(strings.Join(md.EtcdEndpoints, ","))
		g.CarriedKey = md.CarriesAtRestKey
		g.VIP = md.VIP
		g.Generation = md.Generation
	}
	return g, nil
}

func grantEntry(owner *metav1.OwnerReference) (string, bool) {
	if owner == nil {
		return "", false
	}
	tokenID, ok := strings.CutPrefix(owner.Name, tokenSecretPrefix)
	if !ok || tokenID == "" {
		return "", false
	}
	return grantEntryKey(tokenID), true
}

const netconfigEntry = "netconfig"

// filepath.Base of version.CiliumSettingsPath.
const ciliumSettingsEntry = "cilium.json"

// Netconfig comes from netcfg.Load, never the file: a lost file would join on cluster.local.
func collectBundle(rotating bool) (map[string][]byte, error) {
	files := map[string][]byte{}
	for _, f := range sharedCerts {
		b, err := os.ReadFile(filepath.Join(version.PKIDir, f))
		if err != nil {
			if rotating && errors.Is(err, fs.ErrNotExist) && lostToForcedRotation(f) {
				continue
			}
			return nil, fmt.Errorf("read %s: %w", f, err)
		}
		files[f] = b
	}
	inUse, err := pki.KubeletClientCAInUse()
	if err != nil {
		return nil, fmt.Errorf("tell whether the kubelets trust a kubelet-client CA: %w", err)
	}
	for _, f := range rotationCerts {
		required, kubeletClient := kubeletClientEntries[f]
		if f == netconfigEntry || (kubeletClient && !inUse) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(version.PKIDir, f))
		switch {
		case err == nil:
			files[f] = b
		case required:
			return nil, fmt.Errorf("read required split-cluster kubelet client-CA file %s: %w", f, err)
		case kubeletClient && !errors.Is(err, fs.ErrNotExist):
			return nil, fmt.Errorf("read split-cluster kubelet client-CA file %s: %w", f, err)
		case f == ciliumSettingsEntry && !errors.Is(err, fs.ErrNotExist):
			return nil, fmt.Errorf("read the cluster's Cilium settings to share with joining control planes: %w", err)
		}
	}
	secrets, err := profileSecretEntries()
	if err != nil {
		return nil, err
	}
	for _, f := range slices.Sorted(maps.Keys(secrets)) {
		b, err := os.ReadFile(filepath.Join(version.PKIDir, f))
		switch {
		case err == nil:
			files[f] = b
		case !errors.Is(err, fs.ErrNotExist):
			return nil, fmt.Errorf("read the shared secret %s to share with joining control planes: %w", f, err)
		}
	}
	netc, err := netcfg.Load()
	if err != nil {
		return nil, fmt.Errorf("read the cluster network config to share with joining control planes: %w", err)
	}
	files[netconfigEntry] = netc.Encode()
	return files, nil
}

// Grant reader RBAC before the Secret: a failure between must not strand unreadable ciphertext.
func UploadCerts(certKeyHex string, owner *metav1.OwnerReference, confirm func(context.Context) error) error {
	return uploadCerts(certKeyHex, owner, false, false, confirm)
}

func uploadCerts(certKeyHex string, owner *metav1.OwnerReference, rotating, invalidateGrants bool, confirm func(context.Context) error) error {
	if err := requireControlPlanePKI(rotating); err != nil {
		return err
	}
	if err := ensureEtcdTrust(); err != nil {
		return fmt.Errorf("ensure etcd CA before upload: %w", err)
	}
	endpoints, err := publishableEtcdEndpoints(uploadListMembers())
	if err != nil {
		return err
	}
	cs, err := uploadClientset()
	if err != nil {
		return err
	}
	ctx := context.Background()
	files, err := collectBundle(rotating)
	if err != nil {
		return err
	}
	entry, hasGrant := grantEntry(owner)
	carriesKey := false
	if _, ok := files[encryptionConfigEntry]; ok {
		carriesKey = true
	}
	var proof []byte
	if carriesKey {
		raw, err := atRestKeyFromEncryptionConfig(files[encryptionConfigEntry])
		if err != nil {
			return fmt.Errorf("seal the at-rest-key proof for the bundle this upload publishes: %w", err)
		}
		if proof, err = sealAtRestKeyProof(raw); err != nil {
			return err
		}
	}
	sealGrant := func(generation uint64) ([]byte, error) {
		env := bundleEnvelopeV1{Version: bundleEnvelopeVersion, Files: files}
		env.Metadata.Generation = generation
		env.Metadata.EtcdEndpoints = parseEtcdEndpoints(endpoints)
		env.Metadata.CarriesAtRestKey = carriesKey
		env.Metadata.VIP = recordedVIP()
		env.Metadata.Profile = recordedProfile()
		b, err := json.Marshal(env)
		if err != nil {
			return nil, err
		}
		return encrypt(certKeyHex, b)
	}
	flat, err := json.Marshal(files)
	if err != nil {
		return err
	}
	flatCT, err := encrypt(certKeyHex, flat)
	if err != nil {
		return err
	}
	if err := GrantCertsReader(ctx, cs); err != nil {
		return err
	}

	secrets := cs.CoreV1().Secrets("kube-system")
	setAt := time.Now().UTC().Format(time.RFC3339)
	annotations := map[string]string{certKeySetAnnotation: setAt, etcdEndpointsAnnotation: endpoints}

	applyUpdate := func() error {
		return retry.RetryOnConflict(retry.DefaultRetry, func() error {
			cur, err := secrets.Get(ctx, certsSecret, metav1.GetOptions{})
			if err != nil {
				return err
			}
			generation := nextBundleGeneration(cur)
			if cur.Data == nil {
				cur.Data = map[string][]byte{}
			}
			var unread error
			if invalidateGrants {
				for name := range cur.Data {
					if strings.HasPrefix(name, grantEntryPrefix) {
						delete(cur.Data, name)
					}
				}
			} else {
				var expired []string
				expired, unread = expiredGrantEntries(ctx, secrets, cur.Data)
				for _, name := range expired {
					delete(cur.Data, name)
				}
			}
			cur.Data["certs"] = flatCT
			if hasGrant {
				gct, err := sealGrant(generation)
				if err != nil {
					return err
				}
				cur.Data[entry] = gct
			}
			if carriesKey {
				cur.Data[encryptionKeyProofEntry] = proof
			} else {
				delete(cur.Data, encryptionKeyProofEntry)
			}
			if cur.Annotations == nil {
				cur.Annotations = map[string]string{}
			}
			cur.Annotations[certKeySetAnnotation] = setAt
			cur.Annotations[etcdEndpointsAnnotation] = endpoints
			cur.Annotations[bundleGenerationAnnotation] = strconv.FormatUint(generation, 10)
			if owner != nil {
				cur.SetOwnerReferences(mergeOwnerRef(cur.GetOwnerReferences(), *owner))
			}
			if secretDataSize(cur.Data) > grantSecretBudget {
				return overfullGrantsError(cur.Data, entry, unread)
			}
			if confirm != nil {
				// Confirm after all reads, on every retry: a lost lease must refuse the write.
				if err := confirm(ctx); err != nil {
					return fmt.Errorf("the maintenance lease was lost before this publication: %w", err)
				}
			}
			_, err = secrets.Update(ctx, cur, metav1.UpdateOptions{})
			return err
		})
	}

	_, gerr := secrets.Get(ctx, certsSecret, metav1.GetOptions{})
	if gerr != nil && !apierrors.IsNotFound(gerr) {
		return gerr
	}
	if gerr == nil {
		return applyUpdate()
	}
	data := map[string][]byte{"certs": flatCT}
	if hasGrant {
		gct, err := sealGrant(1)
		if err != nil {
			return err
		}
		data[entry] = gct
	}
	if carriesKey {
		data[encryptionKeyProofEntry] = proof
	}
	annotations[bundleGenerationAnnotation] = strconv.FormatUint(1, 10)
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        certsSecret,
			Namespace:   "kube-system",
			Annotations: annotations,
		},
		Data: data,
	}
	if owner != nil {
		sec.OwnerReferences = []metav1.OwnerReference{*owner}
	}
	if confirm != nil {
		if err := confirm(ctx); err != nil {
			return fmt.Errorf("the maintenance lease was lost before this publication: %w", err)
		}
	}
	if _, err := secrets.Create(ctx, sec, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		return applyUpdate()
	}
	return nil
}

// Literal names internal/certs and internal/upgrade also spell for their own acquires.
var maintenanceLeaseNames = []string{"stratum-cluster-maintenance", "stratum-cluster-upgrade"}

// Policy storage alone does not synchronize the admission and RBAC caches.
func GrantCertsReader(ctx context.Context, cs kubernetes.Interface) error {
	if err := ensureJoinFencePolicy(ctx, cs); err != nil {
		return fmt.Errorf("put the %s admission policy in place before granting control-plane join tokens "+
			"the maintenance leases: %w", k8sutil.JoinFencePolicy, err)
	}
	if err := awaitJoinFenceEnforcement(ctx, cs); err != nil {
		return fmt.Errorf("verify join fence admission before granting lease rights: %w", err)
	}
	// A collection POST carries no resource name, so ResourceNames cannot restrict create.
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "stratum:certs-reader", Namespace: "kube-system"},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""}, Resources: []string{"secrets"},
				ResourceNames: []string{certsSecret}, Verbs: []string{"get"},
			},
			{
				APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"},
				ResourceNames: slices.Clone(maintenanceLeaseNames),
				Verbs:         []string{"delete", "get", "update"},
			},
			{
				APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"},
				Verbs: []string{"create"},
			},
		},
	}
	if _, err := cs.RbacV1().Roles("kube-system").Create(ctx, role, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		if err := convergeCertsReaderRole(ctx, cs, role); err != nil {
			return err
		}
	}
	return ensureCertsReaderBinding(ctx, cs)
}

// Only the control-plane bootstrap group; every worker token is in k8sutil.BootstrapGroup.
var certsReaderSubjects = []rbacv1.Subject{{Kind: "Group", Name: k8sutil.ControlPlaneBootstrapGroup, APIGroup: rbacv1.GroupName}}

// An earlier stratum's binding names BootstrapGroup; a worker token could read ciphertext.
func ensureCertsReaderBinding(ctx context.Context, cs kubernetes.Interface) error {
	_, err := cs.RbacV1().RoleBindings("kube-system").Create(ctx, wantCertsReaderBinding(), metav1.CreateOptions{})
	if err == nil || !apierrors.IsAlreadyExists(err) {
		return err
	}
	return narrowCertsReaderBinding(ctx, cs)
}

// Rewrites an existing binding to name only certsReaderSubjects; leaves an absent one absent.
func NarrowCertsReaderBinding(ctx context.Context, cs kubernetes.Interface) error {
	if err := narrowCertsReaderBinding(ctx, cs); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func wantCertsReaderBinding() *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "stratum:certs-reader", Namespace: "kube-system"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "stratum:certs-reader"},
		Subjects:   slices.Clone(certsReaderSubjects),
	}
}

func narrowCertsReaderBinding(ctx context.Context, cs kubernetes.Interface) error {
	rbs := cs.RbacV1().RoleBindings("kube-system")
	want := wantCertsReaderBinding()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, err := rbs.Get(ctx, want.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if cur.RoleRef != want.RoleRef {
			return fmt.Errorf("RoleBinding kube-system/%s refers to %s %q, not Role %q; "+
				"delete it and re-run so it can be recreated", want.Name, cur.RoleRef.Kind, cur.RoleRef.Name, want.RoleRef.Name)
		}
		if slices.Equal(cur.Subjects, want.Subjects) {
			return nil
		}
		cur.Subjects = want.Subjects
		_, err = rbs.Update(ctx, cur, metav1.UpdateOptions{})
		return err
	})
}

func convergeCertsReaderRole(ctx context.Context, cs kubernetes.Interface, want *rbacv1.Role) error {
	roles := cs.RbacV1().Roles("kube-system")
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, err := roles.Get(ctx, want.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if reflect.DeepEqual(cur.Rules, want.Rules) {
			return nil
		}
		cur.Rules = want.Rules
		_, err = roles.Update(ctx, cur, metav1.UpdateOptions{})
		return err
	})
}

func SharedCertsKeySetAt() (setAt time.Time, present bool, err error) {
	cs, err := k8sutil.AdminClientset()
	if err != nil {
		return time.Time{}, false, err
	}
	sec, err := cs.CoreV1().Secrets("kube-system").Get(context.Background(), certsSecret, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	if s := sec.Annotations[certKeySetAnnotation]; s != "" {
		if t, perr := time.Parse(time.RFC3339, s); perr == nil {
			return t, true, nil
		}
	}
	return sec.CreationTimestamp.Time, true, nil
}

func mergeOwnerRef(refs []metav1.OwnerReference, owner metav1.OwnerReference) []metav1.OwnerReference {
	for _, r := range refs {
		if r.UID == owner.UID {
			return refs
		}
	}
	return append(refs, owner)
}

// A missing etcd CA means a build predating the split; a generated one would diverge.
func requireEtcdCA(files map[string][]byte) error {
	if len(files[pki.EtcdCAName+".crt"]) == 0 || len(files[pki.EtcdCAName+".key"]) == 0 {
		return fmt.Errorf("shared certs carry no etcd CA — the control plane that published them is running a stratum build that predates the shared etcd CA. " +
			"Upgrade that node and re-run `stratum token create --control-plane` to republish, then retry")
	}
	return nil
}

// Every grant is sealed at its mint; a kept one would hand a joiner the pre-rotation set.
func ReUploadCerts(certKeyHex string) error { return uploadCerts(certKeyHex, nil, true, true, nil) }

func RekeySharedCerts(certKeyHex string, confirm func(context.Context) error) error {
	return uploadCerts(certKeyHex, nil, false, true, confirm)
}

// Read from pki's manifest rather than spelled, so the map follows what pki stages.
var stagedReplacements = func() map[string]string {
	m := make(map[string]string)
	for _, f := range pki.TrustLayout() {
		if f.Class == pki.TrustShared && f.StagedAs != "" && strings.HasSuffix(f.Name, ".key") {
			m[f.Name] = f.StagedAs
		}
	}
	return m
}()

// A rotation without --force never omits a key, so a lost key must stop the re-upload.
func lostToForcedRotation(name string) bool {
	repl, ok := stagedReplacements[name]
	if !ok || !pki.ForcedRotation() {
		return false
	}
	_, err := os.Stat(filepath.Join(version.PKIDir, repl))
	return err == nil
}

// Runs before the member listing, whose etcd client authenticates with the bundle it mints.
var ensureEtcdTrust = pki.EnsureEtcdTrustBundle

var requireControlPlanePKI = func(rotating bool) error {
	if _, err := os.Stat(filepath.Join(version.PKIDir, pki.RootCAName+".key")); err != nil {
		if rotating && lostToForcedRotation(pki.RootCAName+".key") {
			return nil
		}
		return fmt.Errorf("`stratum token create --control-plane` must run on a control plane: it uploads this "+
			"node's cluster CA material (%s) and reads its local etcd, and this machine has neither (%w). Run it on "+
			"a control-plane node, or mint a worker token (without --control-plane) from here", version.PKIDir, err)
	}
	return nil
}

var uploadListMembers = func() ([]string, error) { return MemberClientIPs("127.0.0.1") }

var uploadClientset = func() (kubernetes.Interface, error) {
	return k8sutil.AdminClientset()
}

// Not best-effort: a mint without a list joins to a VIP etcd cannot answer.
func publishableEtcdEndpoints(ips []string, err error) (string, error) {
	if err != nil {
		return "", fmt.Errorf("list this control plane's etcd members, which a joining control plane dials: %w "+
			"(nothing was changed; run this on a control plane whose etcd is healthy)", err)
	}
	s := formatEtcdEndpoints(parseEtcdEndpoints(formatEtcdEndpoints(ips)))
	if s == "" {
		return "", fmt.Errorf("the local etcd reported no member client addresses, so a joining control plane " +
			"would have none to dial (nothing was changed)")
	}
	return s, nil
}

func formatEtcdEndpoints(ips []string) string { return strings.Join(ips, ",") }

// The value is data a joiner is about to dial; a hostname or URL is not stratum's.

func parseEtcdEndpoints(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if net.ParseIP(f) == nil || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

// clientcmd merges credentials into a config only for TLS transport; read the token directly.
func DownloadCerts(bootstrapKubeconfig, certKeyHex string, check func(bundleProfile) error) (bundleGrant, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", bootstrapKubeconfig)
	if err != nil {
		return bundleGrant{}, err
	}
	apiCfg, err := clientcmd.LoadFromFile(bootstrapKubeconfig)
	if err != nil {
		return bundleGrant{}, fmt.Errorf("read the bootstrap kubeconfig: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return bundleGrant{}, err
	}
	g, err := downloadWith(cs, certKeyHex, bootstrapTokenID(kubeconfigUserToken(apiCfg)), check)
	return g, joinTokenHint(err)
}

var (
	certsReachWait      = 2 * time.Minute
	certsAttemptTimeout = 15 * time.Second
	certsReachInterval  = 2 * time.Second
)

// A just-up apiserver behind the VIP refuses valid tokens until it reads token Secrets.
func getCertsSecret(ctx context.Context, secrets corev1client.SecretInterface, out io.Writer) (*corev1.Secret, error) {
	deadline := time.Now().Add(certsReachWait)
	unreachable := false
	for attempt := 1; ; attempt++ {
		actx, cancel := context.WithTimeout(ctx, min(certsAttemptTimeout, time.Until(deadline)))
		sec, err := secrets.Get(actx, certsSecret, metav1.GetOptions{})
		cancel()
		if err == nil {
			return sec, nil
		}
		if transientReadError(err) {
			unreachable = true
		} else if !unreachable || !apierrors.IsUnauthorized(err) {
			return sec, err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		giveUp := func() error {
			return fmt.Errorf("%w (control plane still unreachable after %s, %d attempts)", err, certsReachWait, attempt)
		}
		left := time.Until(deadline)
		if left <= 0 {
			return nil, giveUp()
		}
		fmt.Fprintf(out, "  control plane not reachable yet (%v); retrying (attempt %d)...\n", err, attempt)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(min(certsReachInterval, left)):
		}
		if time.Until(deadline) <= 0 {
			return nil, giveUp()
		}
	}
}

func transientReadError(err error) bool {
	for _, errno := range []syscall.Errno{syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.EHOSTUNREACH, syscall.ENETUNREACH} {
		if errors.Is(err, errno) {
			return true
		}
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return apierrors.IsServerTimeout(err) || apierrors.IsTimeout(err) || apierrors.IsTooManyRequests(err) || apierrors.IsServiceUnavailable(err)
}

func kubeconfigUserToken(apiCfg *clientcmdapi.Config) string {
	name := apiCfg.CurrentContext
	if name == "" {
		return ""
	}
	ctxDef, ok := apiCfg.Contexts[name]
	if !ok {
		return ""
	}
	auth, ok := apiCfg.AuthInfos[ctxDef.AuthInfo]
	if !ok {
		return ""
	}
	return auth.Token
}

func bootstrapTokenID(token string) string {
	id, _, ok := strings.Cut(token, ".")
	if !ok {
		return ""
	}
	return id
}

func joinTokenHint(err error) error {
	if !apierrors.IsForbidden(err) {
		return err
	}
	return fmt.Errorf("%w (the join token is not a control-plane token: mint one with "+
		"`stratum token create --control-plane` on a control plane running this stratum release)", err)
}

func DownloadCertsAdmin(certKeyHex string) error {
	cs, err := k8sutil.AdminClientset()
	if err != nil {
		return err
	}
	_, err = downloadWith(cs, certKeyHex, "", nil)
	return err
}

func downloadWith(cs *kubernetes.Clientset, certKeyHex, tokenID string, check func(bundleProfile) error) (bundleGrant, error) {
	ctx := context.Background()
	sec, err := getCertsSecret(ctx, cs.CoreV1().Secrets("kube-system"), os.Stdout)
	if err != nil {
		return bundleGrant{}, fmt.Errorf("get %s secret: %w", certsSecret, err)
	}
	plaintext, err := decryptGrant(sec, certKeyHex, tokenID)
	if err != nil {
		return bundleGrant{}, fmt.Errorf("decrypt shared certs (wrong certificate-key?): %w", err)
	}
	g, err := openGrant(sec, plaintext, check)
	if err != nil {
		return bundleGrant{}, err
	}
	entry, ciphertext := grantCiphertext(sec, tokenID)
	g.confirmCurrent = func(ctx context.Context) error {
		current, err := cs.CoreV1().Secrets("kube-system").Get(ctx, certsSecret, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("recheck shared certificate grant: %w", err)
		}
		// Other mints append grants; only this download's entry must stay unchanged.
		if !bytes.Equal(current.Data[entry], ciphertext) {
			return fmt.Errorf("HA join: shared certificate grant changed during preparation; mint a new control-plane join command and re-run (nothing was added to etcd)")
		}
		return nil
	}
	return g, nil
}

// Present but not opening is the wrong key, never a reason to fall back to the legacy entry.
func decryptGrant(sec *corev1.Secret, certKeyHex, tokenID string) ([]byte, error) {
	_, ciphertext := grantCiphertext(sec, tokenID)
	return decrypt(certKeyHex, ciphertext)
}

func grantCiphertext(sec *corev1.Secret, tokenID string) (string, []byte) {
	if tokenID != "" {
		if ct, ok := sec.Data[grantEntryKey(tokenID)]; ok {
			return grantEntryKey(tokenID), ct
		}
	}
	return "certs", sec.Data["certs"]
}

func applyDownloadedBundle(files map[string][]byte) (carriedKey bool, err error) {
	carriedKey, err = applyBundle(version.PKIDir, files)
	if err != nil {
		return false, err
	}
	_, rootBundle := files[pki.RootTrustBundleName]
	_, kubeletBundle := files[pki.KubeletClientBundleBase+"-bundle.crt"]
	if rootBundle && !kubeletBundle {
		if err := pki.SyncKubeletClientBundle(); err != nil {
			return false, err
		}
	}
	return carriedKey, nil
}

// Optional entries are not removed, so an on-disk key does not mean this bundle had one.
func applyBundle(dir string, files map[string][]byte) (carriedKey bool, err error) {
	if err := writeCertFiles(dir, files); err != nil {
		return false, err
	}
	_, carriedKey = files[encryptionConfigEntry]
	return carriedKey, nil
}
