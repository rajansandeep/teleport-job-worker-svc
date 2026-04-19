#!/usr/bin/env bash
set -euo pipefail
umask 077

OUT_DIR="${1:-certs}"
mkdir -p "${OUT_DIR}"

CA_KEY="${OUT_DIR}/ca.key"
CA_CRT="${OUT_DIR}/ca.crt"
SERVER_KEY="${OUT_DIR}/server.key"
SERVER_CRT="${OUT_DIR}/server.crt"
ADMIN_KEY="${OUT_DIR}/admin.key"
ADMIN_CRT="${OUT_DIR}/admin.crt"
VIEWER_KEY="${OUT_DIR}/viewer.key"
VIEWER_CRT="${OUT_DIR}/viewer.crt"
SERIAL="${OUT_DIR}/ca.srl"

# All intermediate files (CSRs, extension configs) go in one temp directory
# so a single rm -rf cleans everything up on exit.
WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT

CA_CSR="${WORKDIR}/ca.csr"
SERVER_CSR="${WORKDIR}/server.csr"
ADMIN_CSR="${WORKDIR}/admin.csr"
VIEWER_CSR="${WORKDIR}/viewer.csr"

CA_EXT="${WORKDIR}/ca.ext"
SERVER_EXT="${WORKDIR}/server.ext"
CLIENT_EXT="${WORKDIR}/client.ext"

printf 'basicConstraints=critical,CA:TRUE\nkeyUsage=critical,keyCertSign,cRLSign\n' > "${CA_EXT}"
printf 'basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=serverAuth\nsubjectAltName=DNS:localhost,IP:127.0.0.1,IP:::1\n' > "${SERVER_EXT}"
printf 'basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=clientAuth\n' > "${CLIENT_EXT}"

generate_ec_key() {
  openssl ecparam -name prime256v1 -genkey -noout -out "$1"
  chmod 600 "$1"
}

echo "Generating CA..."
generate_ec_key "${CA_KEY}"
openssl req -new -key "${CA_KEY}" -out "${CA_CSR}" -subj "/CN=JobWorker CA"
openssl x509 -req \
  -in "${CA_CSR}" \
  -signkey "${CA_KEY}" \
  -out "${CA_CRT}" \
  -days 365 \
  -extfile "${CA_EXT}"

echo "Generating server certificate..."
generate_ec_key "${SERVER_KEY}"
openssl req -new -key "${SERVER_KEY}" -out "${SERVER_CSR}" -subj "/CN=localhost"
openssl x509 -req \
  -in "${SERVER_CSR}" \
  -CA "${CA_CRT}" -CAkey "${CA_KEY}" \
  -CAcreateserial -CAserial "${SERIAL}" \
  -out "${SERVER_CRT}" \
  -days 365 \
  -extfile "${SERVER_EXT}"

generate_client() {
  local name="$1" key="$2" csr="$3" crt="$4"
  echo "Generating client certificate for ${name}..."
  generate_ec_key "${key}"
  openssl req -new -key "${key}" -out "${csr}" -subj "/CN=${name}"
  openssl x509 -req \
    -in "${csr}" \
    -CA "${CA_CRT}" -CAkey "${CA_KEY}" -CAserial "${SERIAL}" \
    -out "${crt}" \
    -days 365 \
    -extfile "${CLIENT_EXT}"
}

generate_client "admin"  "${ADMIN_KEY}"  "${ADMIN_CSR}"  "${ADMIN_CRT}"
generate_client "viewer" "${VIEWER_KEY}" "${VIEWER_CSR}" "${VIEWER_CRT}"

echo "Certificates written to ${OUT_DIR}/"
ls -1 "${OUT_DIR}"
