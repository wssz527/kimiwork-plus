#!/bin/bash
set -euo pipefail

# Creates a reusable self-signed macOS code signing certificate and PKCS#12 archive.
# Keep the generated private key / .p12 outside the Git repository (e.g. CI secret or release machine only).

OUT_DIR="${1:?usage: $0 <output-dir> [common-name]}"
COMMON_NAME="${2:-Kimi Work+ Code Signing}"
P12_PASSWORD="${P12_PASSWORD:-}"
DAYS="${DAYS:-3650}"

mkdir -p "$OUT_DIR"
chmod 700 "$OUT_DIR"

KEY_FILE="$OUT_DIR/codesign.key.pem"
CERT_FILE="$OUT_DIR/codesign.cert.pem"
P12_FILE="$OUT_DIR/codesign.p12"
CONF_FILE="$OUT_DIR/codesign.openssl.cnf"

if [ -e "$KEY_FILE" ] || [ -e "$CERT_FILE" ] || [ -e "$P12_FILE" ]; then
  echo "Signing material already exists in $OUT_DIR; refusing to overwrite." >&2
  exit 1
fi

umask 077
cat > "$CONF_FILE" <<EOF
[req]
distinguished_name = req_dn
x509_extensions = codesign_ext
prompt = no

[req_dn]
CN = $COMMON_NAME
O = Kimi Work+

[codesign_ext]
basicConstraints = critical, CA:FALSE
keyUsage = critical, digitalSignature
extendedKeyUsage = critical, codeSigning
subjectKeyIdentifier = hash
EOF

openssl req -x509 -newkey rsa:2048 -nodes \
  -keyout "$KEY_FILE" \
  -out "$CERT_FILE" \
  -days "$DAYS" \
  -sha256 \
  -config "$CONF_FILE"

export P12_PASSWORD
openssl pkcs12 -export \
  -inkey "$KEY_FILE" \
  -in "$CERT_FILE" \
  -out "$P12_FILE" \
  -name "$COMMON_NAME" \
  -passout env:P12_PASSWORD

rm -f "$CONF_FILE"
chmod 600 "$KEY_FILE" "$CERT_FILE" "$P12_FILE"

SHA1="$(openssl x509 -in "$CERT_FILE" -noout -fingerprint -sha1 | cut -d= -f2 | tr -d ':')"
SHA256="$(openssl x509 -in "$CERT_FILE" -noout -fingerprint -sha256 | cut -d= -f2 | tr -d ':')"
echo "Created self-signed code signing certificate:"
echo "  CN:     $COMMON_NAME"
echo "  SHA-1:  $SHA1"
echo "  SHA-256:$SHA256"
echo "  Cert:   $CERT_FILE"
echo "  PKCS12: $P12_FILE"
