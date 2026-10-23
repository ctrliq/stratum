"""Read a node's security posture straight from the files on disk.

Usage: python3 posture.py ROOT CHECK

ROOT is the node's data directory (/var/lib/stratum). Each CHECK prints one
line beginning "ok" or "bad", followed by the evidence. A check that cannot
read what it needs prints "error ..." and exits 2, so a caller expecting "bad"
never takes an unreadable node for a failing one.

  kubelet-client  kubelet.conf's clientCAFile is pki/kubelet-client-ca-bundle.crt,
                  which holds at least one stratum-kubelet-client-ca CA and
                  nothing else: no CA from pki/ca-bundle.crt, no other
                  certificate.
  etcd-trust      the file etcd's --trusted-ca-file names holds at least one
                  CA, only CAs, and none from pki/ca-bundle.crt.
  masters         no certificate under pki/ or kubeconfig/ other than
                  super-admin's is a non-CA leaf with O=system:masters, in any
                  RDN; a .crt or .pem file must hold a complete certificate.
  front-proxy     on a control plane, the apiserver's --requestheader-allowed-names
                  is exactly front-proxy-client, front-proxy-client.key and
                  front-proxy-ca.key exist, and every front-proxy-*.key is a
                  root-owned regular 0600 file in a root-owned pki/; on a
                  worker, no front-proxy-*.key exists.

Certificates are compared by the sha256 of their DER encoding. Subjects and
CA flags come from openssl, so a field is read the way TLS reads it.
"""

import base64
import glob
import hashlib
import os
import re
import ssl
import stat
import subprocess
import sys

ROOT, CHECK = sys.argv[1:3]
PKI = os.path.join(ROOT, "pki")


class Unevaluated(Exception):
    pass


PEM_CERT = re.compile(r"-----BEGIN CERTIFICATE-----.+?-----END CERTIFICATE-----", re.S)
RDN_DELIMITER = re.compile(r"(?<!\\)[,+]")
KUBELET_CLIENT_CA = "CN=stratum-kubelet-client-ca"


def certs_in(text):
    return PEM_CERT.findall(text)


def attributes(subject):
    return RDN_DELIMITER.split(subject)


def bundle(path):
    with open(path) as f:
        pems = certs_in(f.read())
    out = []
    for pem in pems:
        subject, is_ca = leaf_subject(pem)
        out.append((hashlib.sha256(ssl.PEM_cert_to_DER_cert(pem)).hexdigest(), subject, is_ca))
    return out


def root_fingerprints():
    return {fp for fp, _, _ in bundle(os.path.join(PKI, "ca-bundle.crt"))}


def split_from_root(path):
    have = bundle(path)
    leaves = [subject for _, subject, is_ca in have if not is_ca]
    if leaves:
        return f"bad {path} holds a certificate that is not a CA: {'; '.join(leaves)}"
    if not have:
        return f"bad {path} holds no CA"
    shared = {fp for fp, _, _ in have} & root_fingerprints()
    if shared:
        return f"bad {path} holds {len(shared)} of its {len(have)} CA(s) from ca-bundle.crt"
    return f"ok {path} holds {len(have)} CA(s), none from ca-bundle.crt"


def etcd_trust():
    with open(os.path.join(ROOT, "config", "manifests", "etcd.yaml")) as f:
        m = re.search(r"(?<![\w-])--trusted-ca-file(?:=|['\"]?\s*\n\s*-\s*)['\"]?([^'\"\s]+)", f.read())
    if not m:
        raise Unevaluated("etcd.yaml names no --trusted-ca-file")
    return split_from_root(m.group(1))


def kubelet_client():
    want = os.path.join(PKI, "kubelet-client-ca-bundle.crt")
    with open(os.path.join(ROOT, "config", "kubelet.conf")) as f:
        m = re.search(r"\bclientCAFile:\s*['\"]?([^'\"\s,}]+)", f.read())
    if not m:
        raise Unevaluated("kubelet.conf sets no clientCAFile")
    if m.group(1) != want:
        return f"bad kubelet.conf's clientCAFile is {m.group(1)!r}, not {want}"
    have, roots = bundle(want), root_fingerprints()
    from_root = [subject for fp, subject, _ in have if fp in roots]
    other = [subject for fp, subject, is_ca in have
             if fp not in roots and not (is_ca and KUBELET_CLIENT_CA in attributes(subject))]
    anchors = len(have) - len(from_root) - len(other)
    if from_root:
        return f"bad {want} holds {len(from_root)} of its {len(have)} CA(s) from ca-bundle.crt"
    if other:
        return f"bad {want} holds {'; '.join(other)}, neither from ca-bundle.crt nor a kubelet-client CA"
    if not anchors:
        return f"bad {want} holds no kubelet-client CA"
    return f"ok {want} holds {anchors} kubelet-client CA(s) and none from ca-bundle.crt"


def leaf_subject(pem):
    out = subprocess.run(
        ["openssl", "x509", "-noout", "-subject", "-nameopt", "RFC2253", "-ext", "basicConstraints"],
        input=pem, capture_output=True, text=True, check=True).stdout
    subject, *constraints = out.splitlines()
    return subject.removeprefix("subject=").strip(), any("CA:TRUE" in line for line in constraints)


def kubeconfig_certs(path, text):
    data = re.findall(r"['\"]?client-certificate-data['\"]?\s*:\s*['\"]?([A-Za-z0-9+/=]+)", text)
    pems = [base64.b64decode(d).decode() for d in data]
    for ref in re.findall(r"['\"]?client-certificate['\"]?\s*:\s*['\"]?([^'\"\s,}]+)", text):
        ref = ref if os.path.isabs(ref) else os.path.join(os.path.dirname(path), ref)
        with open(ref) as f:
            found = certs_in(f.read())
        if not found:
            raise Unevaluated(f"{path} names {ref}, which holds no complete certificate")
        pems += found
    return pems


def masters():
    skip = {os.path.join(PKI, "super-admin.crt"), os.path.join(ROOT, "kubeconfig", "super-admin.conf")}
    stray, checked = [], 0
    for top in (PKI, os.path.join(ROOT, "kubeconfig")):
        for d, _, files in os.walk(top, followlinks=True):
            for name in files:
                p = os.path.join(d, name)
                if p in skip:
                    continue
                if not os.path.isfile(p):
                    raise Unevaluated(f"{p} is not a readable regular file")
                with open(p, errors="replace") as f:
                    text = f.read()
                if name.endswith((".crt", ".pem")) or "BEGIN CERTIFICATE" in text:
                    pems = certs_in(text)
                    if not pems:
                        raise Unevaluated(f"{p} holds no complete certificate")
                else:
                    pems = kubeconfig_certs(p, text) if "client-certificate" in text else []
                for pem in pems:
                    subject, is_ca = leaf_subject(pem)
                    if is_ca:
                        continue
                    checked += 1
                    if "O=system:masters" in attributes(subject):
                        stray.append(f"{p} ({subject})")
    if stray:
        return "bad " + "; ".join(stray) + " carry system:masters"
    return f"ok {checked} leaf certificate(s) checked; none but super-admin's carries system:masters"


def confined(path):
    st = os.lstat(path)
    if not stat.S_ISREG(st.st_mode):
        return f"{path} is not a regular file"
    if st.st_uid != 0 or stat.S_IMODE(st.st_mode) != 0o600:
        return f"{path} is uid {st.st_uid} mode {stat.S_IMODE(st.st_mode):o}, not root 0600"
    return ""


def front_proxy():
    keys = sorted(glob.glob(os.path.join(PKI, "front-proxy-*.key")))
    manifest = os.path.join(ROOT, "config", "manifests", "kube-apiserver.yaml")
    if not os.path.exists(manifest):
        return f"bad a worker holds {', '.join(keys)}" if keys else "ok a worker holds no front-proxy key"
    with open(manifest) as f:
        names = re.findall(r"--requestheader-allowed-names=([^\"'\s]*)", f.read())
    if names != ["front-proxy-client"]:
        return f"bad the apiserver's --requestheader-allowed-names is {names!r}, not ['front-proxy-client']"
    missing = [n for n in ("front-proxy-client.key", "front-proxy-ca.key") if os.path.join(PKI, n) not in keys]
    if missing:
        return f"bad {', '.join(missing)} missing from {PKI}"
    if os.stat(PKI).st_uid != 0:
        return f"bad {PKI} is not root-owned"
    problems = [p for p in map(confined, keys) if p]
    if problems:
        return "bad " + "; ".join(problems)
    return f"ok allowed-names is front-proxy-client; {', '.join(os.path.basename(k) for k in keys)} root-owned 0600"


CHECKS = {
    "kubelet-client": kubelet_client,
    "etcd-trust": etcd_trust,
    "masters": masters,
    "front-proxy": front_proxy,
}
try:
    print(CHECKS[CHECK]())
except (Unevaluated, OSError, ValueError, subprocess.CalledProcessError) as e:
    print(f"error cannot evaluate {CHECK}: {e}")
    sys.exit(2)
