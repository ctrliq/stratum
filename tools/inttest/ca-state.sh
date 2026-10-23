#!/usr/bin/env bash
# ca-state.sh PKI_DIR — one controller's CA rotation state, for a failed
# rotation's evidence: the fingerprint of the CA it signs with and of any
# staged one, every CA its trust bundle holds, which CA issued its admin,
# super-admin and kube-vip certificates (by authority key ID, beside the
# signing CA's subject key ID), and the rotation markers it holds. Run on the
# node: check-ha-rotate-ca.sh pipes it to `bash -s` there.
set -u
cd "$1" || exit 1
fp() { openssl x509 -noout -fingerprint -sha256 -in "$1" | cut -d= -f2; }
kid() { openssl x509 -noout -ext "$1" -in "$2" 2>/dev/null | sed -n '2p' | tr -d ' '; }
for f in ca ca-new; do
  [ -f "$f.crt" ] && echo "$f.crt sha256=$(fp "$f.crt") ski=$(kid subjectKeyIdentifier "$f.crt")"
done
tmp=$(mktemp -d)
awk -v d="$tmp" '/-----BEGIN CERTIFICATE-----/{n++} n{print > (d "/" n)}' ca-bundle.crt
for b in "$tmp"/*; do echo "ca-bundle.crt: sha256=$(fp "$b") ski=$(kid subjectKeyIdentifier "$b")"; done
rm -rf "$tmp"
for c in admin super-admin kube-vip; do
  [ -f "$c.crt" ] && echo "$c.crt aki=$(kid authorityKeyIdentifier "$c.crt") $(openssl x509 -noout -subject -nameopt RFC2253 -in "$c.crt")"
done
markers=
for m in carotate-*; do [ -e "$m" ] && markers="$markers $m"; done
echo "markers:${markers:- none}"
