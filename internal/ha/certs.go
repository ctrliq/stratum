// Package ha implements HA control-plane join (`stratum server --server --token
// --certificate-key`): it securely distributes the shared CP certs, adds a live
// etcd member, brings up an additional apiserver, and fronts them with a
// per-node HAProxy.
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

// encryptionConfigEntry is the bundle entry carrying the CIS at-rest key. It is
// derived from version.EncryptionConfigPath rather than spelled again: a bundle
// entry is written to PKIDir/<entry> (writeCertFiles), so the two can only
// agree if the path is PKIDir/<entry>, which TestEncryptionConfigEntryIsTheKeyPath
// pins.
var encryptionConfigEntry = filepath.Base(version.EncryptionConfigPath)

// certKeySetAnnotation records when stratum-certs was last (re-)encrypted under a
// certificate-key. The Secret's own CreationTimestamp does not move on an in-place
// re-key (an Update), so this annotation is what lets `certs check` surface a key
// that has never moved since install.
const certKeySetAnnotation = "stratum.ciq.com/certificate-key-set-at"

// bundleGenerationAnnotation counts the stratum-certs Secret's grant
// publications; the next one is one past it. The generation a joiner reads is
// the copy inside its grant's ciphertext — that one is the authenticated fact
// — but the uploader cannot decrypt the grants it published under earlier
// certificate-keys to count them, so the Secret carries this plain counter
// beside them. It feeds the next publication only: a forged counter can shift
// the number a future grant carries, never change what a published one
// authenticated.
const bundleGenerationAnnotation = "stratum.ciq.com/bundle-generation"

// nextBundleGeneration is the generation the next grant publication gets: one
// past the counter the Secret carries, 1 when it carries none — a Secret
// written before the envelope, whose grants cannot be counted.
func nextBundleGeneration(sec *corev1.Secret) uint64 {
	n, err := strconv.ParseUint(sec.Annotations[bundleGenerationAnnotation], 10, 64)
	if err != nil || n == 0 {
		return 1
	}
	return n + 1
}

// recordedVIP is the cluster VIP this control plane has recorded in its node
// state — the fact bring-up and the join persist, and the one the envelope's
// metadata carries to a joiner. Its absence — a control plane with no saved
// state yet, or none recorded — is metadata absence, never an upload failure:
// a bundle without VIP metadata leaves the joiner's own config in charge,
// exactly as the pre-envelope shape does.
func recordedVIP() string {
	st, err := nodestate.LoadState()
	if err != nil {
		return ""
	}
	return st.VIP.Address
}

// recordedProfile is the node profile this control plane's node state names,
// nil when the state cannot be read; a var so a test can answer for a node it
// did not start. Its absence is metadata absence, as recordedVIP's is.
var recordedProfile = func() *string {
	st, err := nodestate.LoadState()
	if err != nil {
		return nil
	}
	name := st.ProfileName()
	return &name
}

// encryptionKeyProofPlaintext is what the at-rest-key proof seals. The proof
// authenticates the KEY — the AES-GCM open under the raw 32 bytes the key's
// base64 encodes — not a message, so the plaintext is a fixed marker.
const encryptionKeyProofPlaintext = "stratum: this bundle carries the at-rest encryption key"

// sealAtRestKeyProof seals the encryption-key-proof entry under raw. An
// upload whose bundle carries the at-rest key publishes it, so the
// controller-start key gate (internal/cluster, through
// PublishedBundleCarriesKey) can tell by an open — not by an annotation
// anyone with Secret write can forge — that a published bundle would hand a
// joiner this node's key.
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

// etcdEndpointsAnnotation lists the etcd members' client addresses, comma
// separated, as the uploading control plane's etcd reports them. A joining
// control plane dials these for MemberAdd instead of the host in its --join URL,
// which is a VIP in the documented HA shape — and etcd never listens on the VIP,
// so a join through it could not reach etcd at all.
//
// An annotation rather than an entry in the encrypted blob, for two reasons. The
// blob is allowlisted and an older joiner refuses an entry it does not
// know, so adding one would break joining an upgraded cluster with an older
// binary. And it needs no secrecy: these are the node IPs every member already
// advertises, and a tampered list can only point the joiner at an endpoint that
// fails etcd's TLS check against the etcd CA from the encrypted blob.
const etcdEndpointsAnnotation = "stratum.ciq.com/etcd-client-endpoints"

// pkiTrustNames returns the bundle entries pki's trust manifest classifies in
// the given class, in manifest order. pki owns the names — an older joiner
// refuses any entry it does not know (checkCertNames), so the lists below are
// a cross-version contract derived from the manifest rather than spelled here
// — and TestSharedCertSetsAreTheJoinContract pins the derived lists.
func pkiTrustNames(class pki.TrustFileClass) []string {
	var out []string
	for _, f := range pki.TrustLayout() {
		if f.Class == class {
			out = append(out, f.Name)
		}
	}
	return out
}

// sharedCerts must be byte-identical across all control planes (the #1 HA
// footgun): the root CA + its key, the SA signing keypair, and the front-proxy
// CA/client — including the etcd CA, REQUIRED not optional: a joining CP that
// does not receive it runs EnsureCA and mints a DIVERGENT anchor, whose etcd
// the cluster then rejects. UploadCerts guarantees it exists before reading.
// Node-local leaves (apiserver, etcd, kubelet, ...) are regenerated per node
// from these.
var sharedCerts = pkiTrustNames(pki.TrustShared)

// rotationCerts are additionally shared when present: the trust bundles (written
// at install) and any mid-CA-rotation staged material. They are OPTIONAL —
// UploadCerts skips ones that don't exist — so a CP joining during a rotation
// receives the same in-flight trust state as the rest of the cluster. The
// entries after pki's are not pki's files: cluster configuration the join
// bundle carries beside the certificates.
var rotationCerts = append(pkiTrustNames(pki.TrustRotation),
	// Cluster pod/service CIDRs (netcfg): every CP's apiserver/controller-manager
	// must use the same CIDRs + single/dual-stack shape. Not a secret (0644).
	netconfigEntry,
	// The cluster's spec.network.cilium settings, present only when they are
	// not the shipped ones, so a node on an older binary can still join a
	// cluster that never set them. Not a secret (0644).
	ciliumSettingsEntry,
)

// kubeletClientEntries are the rotationCerts shared only from a split or
// migrating cluster, mapped to whether such a cluster must have them. The
// staged -new CA exists only mid-rotation.
var kubeletClientEntries = map[string]bool{
	pki.KubeletClientBundleBase + "-bundle.crt":           true,
	pki.KubeletClientCAName + ".crt":                      true,
	pki.KubeletClientCAName + ".key":                      true,
	pki.StagedTrustName(pki.KubeletClientCAName + ".crt"): false,
	pki.StagedTrustName(pki.KubeletClientCAName + ".key"): false,
}

// certAllowlist is the set of engine entries writeCertFiles will create under
// the PKI directory. It is derived from the same two lists UploadCerts writes
// from, so the download side can never be asked to create a file the upload
// side would not have produced. The registered profiles' shared secrets
// (profileSecretEntries) are the rest.
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

// profileSecretEntries returns the bundle entries the registered profiles'
// shared secrets add. Every profile secret must be every control plane's
// byte-identical copy, so the bundle carries it, written 0600. The cis
// profile's is the at-rest encryption config, encryptionConfigEntry. A secret
// named like an engine entry is refused: the bundle would carry one file for
// both.
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

// carriedSecrets names the profile shared secrets a bundle carries, sorted.
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

// checkCertNames rejects a shared-cert blob carrying any entry outside
// certAllowlist and the registered profiles' shared secrets.
//
// The entry names become filenames under the PKI directory, and filepath.Join
// Cleans its result, so an unvalidated name such as "../../profile.d/x" escapes
// that directory and is written anywhere on the node, as root. Decryption is not
// validation: AES-GCM proves the blob was sealed with the certificate-key, not
// that its contents are well-formed, and the certificate-key is printed to stdout
// in the join command and never rotated.
//
// Rejecting beats skipping. Either one blocks the traversal, but an unexpected
// entry means the blob is not what this code thinks it is, and quietly writing
// the rest of it hides a tampered Secret from the operator. The cost is that a
// node running an older binary refuses a bundle from a newer cluster that added
// a shared file — a loud, named failure rather than a silently incomplete PKI
// directory.
//
// This is a separate pass over the map so that a rejected blob writes NOTHING.
// Go randomises map iteration order, so validating inside the write loop would
// persist an arbitrary subset before failing, and would name a different entry
// on each run.
func checkCertNames(dir string, files map[string][]byte) error {
	secrets, err := profileSecretEntries()
	if err != nil {
		return err
	}
	var bad []string
	for name := range files {
		_, ok := certAllowlist[name]
		ok = ok || secrets[name]
		// filepath.Base(name) != name catches any entry that is not a bare
		// filename. Every allowlist entry is one today, so this cannot reject a
		// legitimate bundle; it is here to keep containment if an entry ever
		// gains a separator.
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

// writeCertFiles validates a decrypted shared-cert blob and writes it into dir.
// Split out of downloadWith so the containment rules are testable without a live
// apiserver.
func writeCertFiles(dir string, files map[string][]byte) error {
	// Two validations, then the writes. checkCertNames answers "is every
	// entry one this cluster uploads"; requireEtcdCA answers "is the
	// material this node needs actually here". Both run before the first
	// write, so a refused bundle writes nothing: a partial write followed by
	// a refusal leaves the PKI directory in a state neither side described.
	// Each entry is then replaced whole, but the set is not: a write that
	// fails part-way leaves the entries before it replaced, and itself too if
	// only its directory sync failed, and the error says which.
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
		// Private keys and the profiles' shared secrets (the cis profile's
		// embeds the at-rest key) are 0600 — a 0644 encryption-config is
		// itself a CIS finding.
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

// errPartBundle matches a writeCertFiles that replaced some of a bundle's
// entries and not the rest. What to do next depends on the caller.
var errPartBundle = errors.New("this node holds part of the bundle")

// writeCertFile is host.WriteFileAtomically behind a seam, so a test can fail
// one entry's write.
var writeCertFile = host.WriteFileAtomically

// NewCertKey returns a fresh AES-256 certificate-key (hex) for upload-certs.
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

// grantEntryPrefix is the stratum-certs data-key prefix that separates one
// grant's own ciphertext entry from the legacy single-bundle one in the
// multi-entry grant Secret: every pending control-plane join holds the entry
// grantEntryKey names for its token, and no upload ever replaces another
// grant's.
const grantEntryPrefix = "certs-"

// grantEntryKey is the stratum-certs data key that carries one live grant's
// ciphertext: grantEntryPrefix plus the bootstrap token's ID — the part of the
// token Secret's name bootstrap-token-<id> after its prefix. The joiner
// derives the same name from its own token (bootstrapTokenID), so a download
// opens the entry the mint that printed its certificate-key wrote.
func grantEntryKey(tokenID string) string { return grantEntryPrefix + tokenID }

// tokenSecretPrefix is a bootstrap-token Secret's name prefix: the token ID
// after it is the one a grant's entry is keyed by (grantEntryKey).
const tokenSecretPrefix = "bootstrap-token-"

// expiredGrantEntries is the one rule for which stratum-certs entries have
// outlived their grant: a grant entry whose bootstrap-token Secret is gone —
// the token expired and the apiserver's token cleaner deleted it, or it was
// revoked. Only grant entries are candidates: the legacy certs entry and the
// at-rest-key proof are never grants. A token that cannot be read is never
// taken for gone, because an API hiccup must not drop a live grant: its entry
// is kept, and the read error is returned beside the entries that did expire.
// Every mint applies the rule inside its own update, and the operator applies
// it on each pass (PruneExpiredGrants).
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
			// The token is live: its grant stays for as long as it does.
		case apierrors.IsNotFound(err):
			expired = append(expired, entry)
		default:
			errs = errors.Join(errs, err)
		}
	}
	return expired, errs
}

// PruneExpiredGrants drops the stratum-certs grant entries whose bootstrap
// token is gone (expiredGrantEntries), and leaves the legacy certs entry,
// every live grant's entry and every owner reference as read. The operator
// runs it on every pass, so expired grants go even when no one mints. It
// writes nothing when nothing expired: rewriting the cluster's CA-key
// ciphertext on every pass would bump its resourceVersion without changing
// anything. The write is the object as read minus the expired entries,
// carrying the resourceVersion it read, so a mint landing meanwhile
// conflicts, and the retry re-reads and prunes again from the fresh state. An
// absent Secret — nothing minted yet, or the bundle deleted deliberately — is
// left absent.
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

// grantSecretBudget is the most data a mint lets stratum-certs hold, counting
// every entry's name and value (secretDataSize). The apiserver refuses a
// Secret whose data values total more than corev1.MaxSecretSize, and its
// refusal says only that the data is too long. The budget stays 64 KiB under
// that limit, so the mint that would fill the Secret is refused here, naming
// the grants that fill it and the remedy, and never by the apiserver,
// whatever else the apiserver's count takes in.
const grantSecretBudget = corev1.MaxSecretSize - 64<<10

// secretDataSize is a Secret data map's size as grantSecretBudget counts it.
func secretDataSize(data map[string][]byte) int {
	n := 0
	for name, v := range data {
		n += len(name) + len(v)
	}
	return n
}

// overfullGrantsError refuses a mint whose grant would bring stratum-certs
// past grantSecretBudget. Each grant is a full copy of the shared
// certificates, so the Secret holds a few dozen at most: the refusal names
// the live grants already there by token ID — `stratum token list` does not
// say which tokens are control-plane ones — and the ways to free room. own is
// this mint's own entry, which data already holds ("" for an upload with no
// grant), and unread is the token reads that kept a grant only because they
// failed.
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

// bundleEnvelopeVersion is the envelope version this build publishes and
// decodes. It is the cross-version contract's v1: a future stratum must
// decode what it publishes, and this build must refuse (never misread) an
// envelope version it does not know.
const bundleEnvelopeVersion = 1

// bundleMetadata is the v1 envelope's metadata section. It travels INSIDE the
// ciphertext, so every fact it carries is authenticated by the same AES-GCM
// open that authenticates the file set — the property an unauthenticated
// Secret annotation cannot have.
type bundleMetadata struct {
	// Generation is the publication's generation: 1 for the first grant an
	// upload publishes, one past the previous publication's.
	Generation uint64 `json:"generation"`
	// EtcdEndpoints is the etcd members' client address list, as the
	// uploading control plane's etcd reported it. It moves here out of the
	// unauthenticated Secret annotation, which stays published only for
	// joiners running a released stratum.
	EtcdEndpoints []string `json:"etcdEndpoints"`
	// CarriesAtRestKey says whether the bundle's files carry the CIS at-rest
	// encryption key — the fact the join's key gate decides on, claimed here
	// by the same ciphertext that carries (or does not carry) the key.
	CarriesAtRestKey bool `json:"carriesAtRestKey"`
	// VIP is the cluster VIP the publishing control plane has recorded in its
	// node state, "" when it recorded none: the authenticated copy of a fact
	// a joining control plane must not take from its own config unchecked.
	VIP string `json:"vip,omitempty"`
	// Profile is the node profile the publishing control plane's node state
	// names, "" for none, and nil when its node state could not be read: a
	// joining control plane refuses a profile other than the cluster's before
	// it writes the bundle. Absent from a bundle an earlier stratum
	// published, which leaves the comparison to the shared secrets.
	Profile *string `json:"profile,omitempty"`
}

// bundleEnvelopeV1 is the v1 envelope around a grant entry's payload: the
// versioned shape the bare flat map grew into. The JSON names are the
// cross-version contract — a future stratum must decode what this shape
// publishes, and this stratum must decode the legacy flat map beside it.
type bundleEnvelopeV1 struct {
	Version  int               `json:"version"`
	Metadata bundleMetadata    `json:"metadata"`
	Files    map[string][]byte `json:"files"`
}

// decodeGrantPayload splits a decrypted grant payload into its file set and
// its envelope metadata. The payload is the v1 envelope when it carries one,
// and the legacy flat map otherwise — the pre-envelope shape every supported
// joiner version decodes, which the legacy `certs` entry keeps published. An
// envelope of a version this build does not support is refused, never decoded
// as v1 or as the flat map: a future format's fields must not be read as v1's.
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

// bundleGrant is the grant a download makes of the bundle: the facts the
// envelope's authenticated metadata carries, and the legacy shape's answers
// when the bundle predates the envelope. It is one value so the join reads
// every bundle fact from the same download, and a joiner of either bundle
// shape gets the same fields.
type bundleGrant struct {
	// Published is the etcd client endpoints the join dials: the envelope's
	// when it carries them, else the legacy annotation's.
	Published []string
	// CarriedKey is whether the bundle carried the at-rest key — the bool the
	// download reported before the envelope — the fact the join's CIS key
	// gate decides on.
	CarriedKey bool
	// VIP is the cluster VIP the publishing control plane recorded, "" when
	// none: the authenticated fact the join takes the cluster's API address
	// from.
	VIP string
	// Generation is the publication's generation, 0 from a legacy flat-map
	// bundle.
	Generation uint64
	bundleProfile
	// confirmCurrent rechecks the exact ciphertext this download applied.
	// The join calls it under the maintenance fence before admitting a member.
	confirmCurrent func(context.Context) error
}

// bundleProfile is what a downloaded bundle says about the cluster's profile.
type bundleProfile struct {
	// Secrets are the profile shared secrets the bundle carried, sorted: a
	// fact about the download, not about the disk, where a secret left by an
	// earlier failed join would survive a bundle without it.
	Secrets []string
	// Profile is the publishing control plane's profile, "" for none, when
	// ProfileKnown: a legacy flat-map bundle, or an envelope an earlier
	// stratum published, does not say.
	Profile      string
	ProfileKnown bool
}

// openGrant decodes a decrypted grant payload, applies its file set to this
// node's PKI, and returns the grant the payload makes of the download. From
// the envelope it takes the authenticated metadata: the etcd endpoints a
// joiner dials — the annotation's list is only the legacy shape's fallback,
// never an equal source — the cluster VIP, the publication's generation, and
// the at-rest-key fact. From the legacy flat map it takes what that shape
// always carried: the annotation's endpoints, whether the files carry the
// key, and no VIP or generation.
//
// check, when non-nil, sees what the bundle says about the cluster's profile
// before any file is written, so a join it refuses leaves this node's PKI as
// it was.
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

// grantEntry returns the stratum-certs entry name an upload writes for its
// token owner, and whether the owner is one: only a bootstrap-token Secret
// (name bootstrap-token-<id>) carries a token ID to key an entry with. The
// CA-rotation re-upload (ReUploadCerts) passes no owner at all, and an owner
// of any other kind has no grant to append.
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

// netconfigEntry is the bundle entry that carries the cluster's network config.
const netconfigEntry = "netconfig"

// ciliumSettingsEntry is the bundle entry that carries the cluster's Cilium
// settings: filepath.Base of version.CiliumSettingsPath.
const ciliumSettingsEntry = "cilium.json"

// collectBundle reads the shared control-plane material from this node. The
// netconfig entry is built from netcfg.Load, never read from the file: a control
// plane that lost the file and has not restarted would otherwise send a joiner no
// netconfig, and the joiner would recover cluster.local and join on the wrong
// domain with no error. Load recovers the domain from node state, and
// its error is returned rather than defaulted.
//
// rotating is the CA rotation's re-upload: an old signing key that a rotation
// recorded as forced went ahead without is left out when its staged
// replacement is there, and a follower that applies the bundle keeps its own.
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

// UploadCerts encrypts the shared CP certs into the stratum-certs Secret and grants
// the control-plane bootstrap group read access (so a joining CP can fetch them
// with its control-plane bootstrap token, then decrypt with the certificate-key
// out of band; a worker token cannot read them).
//
// The Secret is the multi-entry grant Secret: ONE Secret, one ciphertext entry
// per live grant keyed by token ID (certs-<token-id>) beside the legacy certs
// entry. This upload APPENDS its own grant's entry — carrying the v1 envelope,
// the bundle's file set with the authenticated generation, at-rest-key, VIP and
// etcd-endpoint metadata — and re-encrypts the legacy certs entry under its own
// key as the flat map every supported joiner version decodes; it never replaces
// another live grant's entry, so minting a second control-plane join does
// not break the first — each pending grant decrypts its own entry with the
// certificate-key its mint printed while its bootstrap token is live, until a
// rotation, migration or rekey re-upload (ReUploadCerts, RekeySharedCerts)
// retires every grant. The legacy certs entry follows the newest mint's key, so
// a joiner running an older stratum release, which reads only that entry, can
// use only the most recently minted join command. The upload drops the entry of
// every grant whose bootstrap token is gone (expiredGrantEntries) in the same
// update that appends its own — the operator does the same on each pass
// (PruneExpiredGrants), but may be turned off — and refuses, before writing
// anything, a mint whose grant the Secret cannot hold (grantSecretBudget).
//
// owner, when non-nil, is added (deduped by UID) to the Secret's OwnerReferences —
// the bootstrap-token Secret minted in the same `token create --control-plane` run.
// Garbage collection removes stratum-certs only once EVERY owning token is gone, so
// the shared-cert material is reaped when the last granting token expires but is not
// pulled out from under a second, still-valid control-plane join whose own token is
// still live. Owner refs are merged, never replaced, precisely so a later
// `token create` cannot shorten an earlier token's access. The CA-rotation re-upload
// path (ReUploadCerts) passes nil and preserves whatever owners the Secret carries.
// UploadCerts publishes the shared certificates under certKeyHex, tied to
// owner when provided. confirm is the caller's maintenance-lease recheck (nil
// when the caller holds no fence): it runs immediately before the Secret
// write, inside the conflict retry, so a lease lost while an earlier call
// stalled refuses the publication instead of overwriting the successor's
// bundle with this node's snapshot.
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
	// The v1 envelope's authenticated metadata: the publication's generation,
	// the etcd endpoints, the at-rest-key fact, and the cluster VIP this
	// control plane has recorded. The grant's own entry carries it inside the
	// ciphertext; the legacy certs entry keeps the flat map and the annotation
	// keeps the endpoint list, so every supported joiner version still decodes
	// the bundle.
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
	// RBAC first: a Role/RoleBinding with no Secret is inert, so writing them before
	// the Secret means a failure between the two can never strand a stratum-certs
	// Secret that is encrypted under a certificate-key nobody printed AND has no
	// reader grant — unremovable by token revocation and (without an ownerRef) never
	// garbage-collected.
	if err := GrantCertsReader(ctx, cs); err != nil {
		return err
	}

	// The Secret last, tied to owner when provided (see the doc comment).
	// The legacy certs entry carries the flat map under this mint's key for
	// the negotiated transition, and the grant's own entry carries the v1
	// envelope — the same file set, with the authenticated metadata — under
	// the key that mint printed.
	secrets := cs.CoreV1().Secrets("kube-system")
	setAt := time.Now().UTC().Format(time.RFC3339)
	annotations := map[string]string{certKeySetAnnotation: setAt, etcdEndpointsAnnotation: endpoints}

	// A mint's update is an APPEND on the data map: this upload's own entries —
	// the legacy certs entry, its grant's envelope under its generation, and
	// the at-rest-key proof — are set, every other live grant's entry is left
	// exactly as read, and the entry of a grant whose bootstrap token is gone
	// is dropped (expiredGrantEntries), so the Secret does not fill up with
	// expired grants on a cluster that runs no operator. A mint the Secret
	// still cannot hold is refused before anything is written. A rotation
	// re-upload sets only the legacy entry, drops a stale proof, and retires
	// every grant: each one is a snapshot of the trust the rotation just
	// changed. The update carries the resourceVersion it read and the retry
	// re-reads on a conflict, so two control planes minting at once converge
	// instead of one mint's grant entry clobbering the other's.
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
			// An explicit rekey and a rotation re-upload revoke every old
			// certificate-key in the same compare-and-swap that publishes the
			// replacement ciphertext. Token mints keep pending grants intact,
			// and drop only those whose token is gone; a token that cannot be
			// read keeps its grant, and the mint goes ahead.
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
			// The proof vouches for the bundle THIS upload published: a keyed
			// upload publishes it, and a keyless one leaves no stale one
			// behind.
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
			// Both the Secret read and the grant-pruning reads can stall
			// through a takeover. Confirm after all reads and preparation,
			// immediately before writing, including every conflict retry.
			if confirm != nil {
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
		// A Secret appeared between the read and the create: append to it the
		// way the pre-existing case would have.
		return applyUpdate()
	}
	return nil
}

// maintenanceLeaseNames are the two leases the cluster's maintenance fence
// holds (k8sutil.MaintenanceFence): the leases the stratum:certs-reader Role
// grants join tokens, and those a refused join fence take reads back. The
// names are literals, the cross-package contract internal/certs and
// internal/upgrade also spell for their own acquires.
var maintenanceLeaseNames = []string{"stratum-cluster-maintenance", "stratum-cluster-upgrade"}

// GrantCertsReader writes the stratum:certs-reader grant every upload
// publishes before the Secret: the Role and the RoleBinding that let the
// control-plane bootstrap group get the stratum-certs Secret and hold the
// maintenance fence, and, before either, the admission policy that keeps that
// group to its own fence (ensureJoinFencePolicy). The grant waits for
// enforcement on every serving API server before writing the Role; policy
// storage alone does not synchronize the admission and RBAC caches.
func GrantCertsReader(ctx context.Context, cs kubernetes.Interface) error {
	if err := ensureJoinFencePolicy(ctx, cs); err != nil {
		return fmt.Errorf("put the %s admission policy in place before granting control-plane join tokens "+
			"the maintenance leases: %w", k8sutil.JoinFencePolicy, err)
	}
	if err := awaitJoinFenceEnforcement(ctx, cs); err != nil {
		return fmt.Errorf("verify join fence admission before granting lease rights: %w", err)
	}
	// The second rule is the maintenance-lease grant: during the member-add
	// window the joiner's only working cluster credential is its bootstrap
	// token, in the control-plane bootstrap group and nothing else, so the
	// fence its join takes there can be created, renewed and released only if
	// the mint that published its grant granted that group access to the
	// leases. Get, update and delete are restricted to the two maintenance
	// lease names. Create must be separate: Kubernetes authorizes the
	// collection POST with no resource name, so ResourceNames cannot restrict
	// it. RBAC alone would therefore let any join token create any lease in
	// this namespace, and overwrite or delete these two whoever holds them; the
	// admission policy above admits only the two names, stratum's lease
	// duration, and a holder that names the token, over nothing another run
	// holds.
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
		// A Role written by an earlier stratum predates the lease grant, and a
		// joiner under it could take no fence: converge the rules to the ones
		// above, so any fresh mint repairs the grant before any join needs it.
		if err := convergeCertsReaderRole(ctx, cs, role); err != nil {
			return err
		}
	}
	return ensureCertsReaderBinding(ctx, cs)
}

// certsReaderSubjects are the only subjects the stratum:certs-reader RoleBinding
// may name: the control-plane bootstrap group, never k8sutil.BootstrapGroup,
// which every worker token is in.
var certsReaderSubjects = []rbacv1.Subject{{Kind: "Group", Name: k8sutil.ControlPlaneBootstrapGroup, APIGroup: rbacv1.GroupName}}

// ensureCertsReaderBinding creates the stratum:certs-reader RoleBinding, or
// rewrites an existing one whose subjects are not exactly certsReaderSubjects.
//
// Create-if-absent is not enough. A cluster set up by an earlier stratum carries
// the binding with k8sutil.BootstrapGroup as its subject, and leaving it in place
// would keep the stratum-certs ciphertext readable by any worker token. Every
// path that writes that Secret comes through UploadCerts, which calls this
// first, so the old grant is gone before any new ciphertext exists.
func ensureCertsReaderBinding(ctx context.Context, cs kubernetes.Interface) error {
	_, err := cs.RbacV1().RoleBindings("kube-system").Create(ctx, wantCertsReaderBinding(), metav1.CreateOptions{})
	if err == nil || !apierrors.IsAlreadyExists(err) {
		return err
	}
	return narrowCertsReaderBinding(ctx, cs)
}

// NarrowCertsReaderBinding rewrites an existing stratum:certs-reader RoleBinding
// to name only certsReaderSubjects, and leaves an absent one absent. Every token
// mint calls it, so an upgraded cluster drops the worker grant on an existing
// stratum-certs Secret the first time it mints any token, not only on the next
// upload.
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

// convergeCertsReaderRole rewrites an existing stratum:certs-reader Role whose
// rules predate a rule this build publishes — today the maintenance-lease
// grant the joiner's bootstrap token needs to hold the member-add fence — so
// any fresh mint repairs the grant before any join needs it. The same
// create-only gap narrowCertsReaderBinding reconciles, on the rules side; the
// retry re-reads on a conflict the way it does.
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

// SharedCertsKeySetAt reports when the stratum-certs certificate-key was last set
// — the annotation UploadCerts stamps on every re-key — falling back to the
// Secret's CreationTimestamp for a Secret written before that annotation existed.
// present is false when the Secret does not exist. `certs check` uses this to
// surface a key that has not moved since install.
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

// mergeOwnerRef adds owner to refs unless a ref with the same UID is already
// present, so re-uploading under a new token widens the set of tokens that keep
// stratum-certs alive rather than narrowing it to the newest one.
func mergeOwnerRef(refs []metav1.OwnerReference, owner metav1.OwnerReference) []metav1.OwnerReference {
	for _, r := range refs {
		if r.UID == owner.UID {
			return refs
		}
	}
	return append(refs, owner)
}

// requireEtcdCA fails fast when the downloaded material carries no etcd CA,
// rather than letting this node silently mint a divergent anchor.
//
// An older control plane (pre-etcd-CA-split binary) publishes a stratum-certs Secret
// without etcd-ca.*. A joining node would then generate its own in
// pki.GenerateAll — an etcd CA the rest of the cluster does not trust — and the
// failure would surface much later, far from the cause, after the etcd data dir
// had already been wiped.
func requireEtcdCA(files map[string][]byte) error {
	if len(files[pki.EtcdCAName+".crt"]) == 0 || len(files[pki.EtcdCAName+".key"]) == 0 {
		return fmt.Errorf("shared certs carry no etcd CA — the control plane that published them is running a stratum build that predates the shared etcd CA. " +
			"Upgrade that node and re-run `stratum token create --control-plane` to republish, then retry")
	}
	return nil
}

// ReUploadCerts re-encrypts the current on-disk shared material — including the
// trust bundles and any mid-rotation staged CAs — into the stratum-certs Secret, so
// a CP joining after a CA rotation receives the new material. It is UploadCerts
// re-run (create-or-update), except that an old signing key a rotation recorded
// as forced went ahead without is left out when its staged replacement is there.
// It passes no owner, so it rewrites the legacy certs entry under its key and
// keeps the owner references as read, and it removes every grant entry in the
// same update. A grant is the shared files sealed at its mint, under a key
// only its join command holds, so nothing can re-seal it with the rotated
// trust: kept, it would hand a joiner the pre-rotation set — during a halted
// rotation a joiner whose pin still matches the old root would join without
// the staged CA and strand the resumed run. A join command minted before the
// rotation therefore fails at decryption, before it changes anything, and a
// new one is minted from the current material.
// ReUploadCerts publishes under the CA rotation's own fence: the rotation holds and
// renews the lease for its whole run, and its phase machinery pins what each
// phase applied, so no per-write recheck here — the rotation engine owns the
// fence and its resumability.
func ReUploadCerts(certKeyHex string) error { return uploadCerts(certKeyHex, nil, true, true, nil) }

// RekeySharedCerts replaces the shared bundle and removes all per-token
// ciphertexts in one Secret update, so no previous certificate-key can still
// decrypt a pending grant. Like rotation's ReUploadCerts, this explicit
// operator action intentionally invalidates previously issued join keys.
// The caller must hold the maintenance fence and pass the rekey guard first,
// and hand the fence's recheck in as confirm: the rekey makes several API
// calls between the take and the Secret write, and a lease lost while one of
// them stalled must refuse the publication.
func RekeySharedCerts(certKeyHex string, confirm func(context.Context) error) error {
	return uploadCerts(certKeyHex, nil, false, true, confirm)
}

// stagedReplacements maps each old signing key in sharedCerts to the key a CA
// rotation stages to replace it — read from pki's manifest (StagedAs) rather
// than spelled here, so the map follows whatever pki stages.
var stagedReplacements = func() map[string]string {
	m := make(map[string]string)
	for _, f := range pki.TrustLayout() {
		if f.Class == pki.TrustShared && f.StagedAs != "" && strings.HasSuffix(f.Name, ".key") {
			m[f.Name] = f.StagedAs
		}
	}
	return m
}()

// lostToForcedRotation reports whether name is an old signing key that a
// rotation recorded as forced went ahead without: the forced marker is there
// and so is the key's staged replacement. A rotation started without --force
// never omits a key, so a key lost under it stops the re-upload rather than
// publishing a bundle a later rollback would split the cluster over.
func lostToForcedRotation(name string) bool {
	repl, ok := stagedReplacements[name]
	if !ok || !pki.ForcedRotation() {
		return false
	}
	_, err := os.Stat(filepath.Join(version.PKIDir, repl))
	return err == nil
}

// ensureEtcdTrust is pki.EnsureEtcdTrustBundle, indirected for tests. It runs
// before the member listing, whose etcd client authenticates with the bundle it
// mints on a control plane installed before the etcd CA existed.
var ensureEtcdTrust = pki.EnsureEtcdTrustBundle

// requireControlPlanePKI checks for the shared CA material UploadCerts reads;
// mid-rotation, the staged root key stands in for an old one a rotation
// recorded as forced went ahead without. A var so tests can stand in a control plane.
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

// uploadListMembers reads the etcd members' client addresses from this control
// plane's own etcd. A var so tests can stand in for etcd.
var uploadListMembers = func() ([]string, error) { return MemberClientIPs("127.0.0.1") }

// uploadClientset is the apiserver client UploadCerts writes the Secret and its
// RBAC through — k8sutil.AdminClientset, indirected as a var (like
// requireControlPlanePKI, ensureEtcdTrust and uploadListMembers above) so a
// test can stand in a cluster without admin credentials.
var uploadClientset = func() (kubernetes.Interface, error) {
	return k8sutil.AdminClientset()
}

// publishableEtcdEndpoints turns the local member listing into the annotation
// value, or refuses.
//
// Publishing is not best-effort. A token minted without the
// list prints a join command that a VIP join then refuses (joinEtcdEndpoints),
// and on an existing Secret the update would have replaced a good list with
// none. `token create --control-plane` runs on a control plane, whose own etcd
// is the one being asked, so failing here means the cluster is not ready to
// take a new member anyway, and saying so beats minting a join that cannot work.
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

// formatEtcdEndpoints and parseEtcdEndpoints are the annotation's two halves.
// Parsing keeps only literal IPs, deduplicated, in order: the value is data a
// joiner is about to dial, and a hostname or a URL in it is not something any
// stratum wrote.
func formatEtcdEndpoints(ips []string) string { return strings.Join(ips, ",") }

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

// DownloadCerts fetches + decrypts the shared certs (authenticating with the
// bootstrap-kubeconfig token), writes them to version.PKIDir, and returns the
// grant the bundle makes of the download: the etcd client addresses the
// cluster published, whether the bundle itself carried the at-rest encryption
// key — a fact about the download, not about the disk, where a key left by an
// earlier failed join would survive a keyless bundle — the cluster VIP the
// publishing control plane recorded, and the publication's generation. From a
// bundle in the v1 envelope the facts come from the ciphertext's metadata;
// from a legacy flat-map bundle — published before the envelope, or fetched
// with admin credentials by an already-joined control plane, which has no
// token ID — from the shapes that bundle always carried.
//
// The entry decrypted is the grant's own: the bootstrap token
// in the kubeconfig names the certs-<token-id> entry the mint that printed
// this certificate-key wrote, so a second mint moving the legacy entry to a
// newer key does not break this join. The token is read from the kubeconfig's
// own auth info rather than the built client config: clientcmd merges user
// credentials into a config only when its transport is TLS, and the token ID
// must be known whatever endpoint serves the Secret. check is openGrant's.
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

// certsReachWait bounds how long a read of stratum-certs keeps retrying a
// control plane it cannot reach, certsAttemptTimeout bounds one read, and
// certsReachInterval paces the attempts. Vars so a test need not wait them
// out.
var (
	certsReachWait      = 2 * time.Minute
	certsAttemptTimeout = 15 * time.Second
	certsReachInterval  = 2 * time.Second
)

// getCertsSecret reads stratum-certs, retrying for certsReachWait while the
// control plane cannot be reached or answers that it is briefly unable to.
// A join reads it through the VIP, which can move while the previous control
// plane settles, and discovery a moment earlier already retries the same
// way; one refused connection must not end the join. An answer that is a
// decision — forbidden, not found, unauthorized — returns at once, except an
// unauthorized that follows an unreachable control plane: the apiserver that
// has just come up behind the VIP refuses a valid bootstrap token until it
// has read the token Secrets, so that answer is still the outage. Each read
// carries its own deadline inside the budget, so a read that stalls cannot
// outlast it, and no read starts once the budget is spent.
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

// transientReadError reports whether a failed read is the control plane being
// briefly out of reach rather than its answer: a dial refused, reset or
// unroutable, a timeout, or the apiserver saying it is overloaded or not
// ready.
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

// kubeconfigUserToken returns the token of the kubeconfig's current-context
// user, "" when the context or its user names no token.
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

// bootstrapTokenID returns the ID half of a bootstrap token spelled
// <id>.<secret> — the credential the join's bootstrap kubeconfig carries, and
// the key its grant entry is published under. "" when the credential is not a
// bootstrap token: DownloadCertsAdmin has no token at all, and a token without
// the separator names no grant entry, so both take the legacy fallback.
func bootstrapTokenID(token string) string {
	id, _, ok := strings.Cut(token, ".")
	if !ok {
		return ""
	}
	return id
}

// joinTokenHint explains a Forbidden read of stratum-certs on a join, where the
// credential is the bootstrap token: only a control-plane token may read it.
func joinTokenHint(err error) error {
	if !apierrors.IsForbidden(err) {
		return err
	}
	return fmt.Errorf("%w (the join token is not a control-plane token: mint one with "+
		"`stratum token create --control-plane` on a control plane running this stratum release)", err)
}

// DownloadCertsAdmin fetches + decrypts the shared certs using this node's
// admin.kubeconfig. For an already-joined control plane pulling freshly-staged
// CA-rotation material (it has no bootstrap token, but does have admin creds).
// With no token there is no grant entry of its own, so it decrypts the legacy
// certs entry — the newest mint's or the rotation re-upload's key.
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
		// Other mints append grants and replace the legacy entry. Only this
		// download's entry must remain unchanged; a global generation check
		// would wrongly invalidate the earlier, still-live grants.
		if !bytes.Equal(current.Data[entry], ciphertext) {
			return fmt.Errorf("HA join: shared certificate grant changed during preparation; mint a new control-plane join command and re-run (nothing was added to etcd)")
		}
		return nil
	}
	return g, nil
}

// decryptGrant opens the stratum-certs entry this caller decrypts: the grant's
// own certs-<token-id> entry when the caller carries a bootstrap token, the
// legacy certs entry otherwise — and also when the token's entry is absent,
// because the Secret predates the multi-entry format or the caller holds admin
// credentials. An entry that IS present but does not open is
// refused, never fallen back from: that entry is this token's own, so a key
// that does not open it is the wrong certificate-key, and the legacy entry
// under a newer mint's key must not quietly stand in for it.
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

// applyDownloadedBundle applies a downloaded bundle to this node's PKI and,
// when it carries ca-bundle.crt but not the kubelet's bundle, rewrites the
// kubelet's clientCAFile from it: an HA join, a follower's rotation fetch and
// an etcd-CA migration fetch all change the root trust this way, and the
// kubelet must follow.
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

// applyBundle writes a decrypted bundle into dir and reports whether it carried
// the at-rest key. Optional entries a bundle leaves out are not removed from
// dir, so whether the key is on disk afterwards says nothing about whether this
// bundle had one.
func applyBundle(dir string, files map[string][]byte) (carriedKey bool, err error) {
	if err := writeCertFiles(dir, files); err != nil {
		return false, err
	}
	_, carriedKey = files[encryptionConfigEntry]
	return carriedKey, nil
}
