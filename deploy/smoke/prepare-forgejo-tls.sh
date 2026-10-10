#!/usr/bin/env bash
# Generate a short-lived local CA and a Forgejo HTTPS test certificate.
set -euo pipefail
cd "$(dirname "$0")/../.."
umask 077
directory=.local/forgejo-tls
mkdir -p "$directory"
if [[ -e "$directory/ca.key" || -e "$directory/server.key" ]]; then
  printf 'TLS test material already exists; refusing to overwrite it.\n' >&2
  exit 1
fi
openssl req -x509 -newkey rsa:3072 -noenc -days 30 \
  -subj '/CN=SpawnPod local smoke CA' \
  -addext 'basicConstraints=critical,CA:TRUE' \
  -addext 'keyUsage=critical,keyCertSign,cRLSign' \
  -keyout "$directory/ca.key" -out "$directory/ca.crt"
openssl req -new -newkey rsa:2048 -noenc \
  -subj '/CN=forgejo.local' \
  -keyout "$directory/server.key" -out "$directory/server.csr"
cat > "$directory/server.ext" <<'EOF'
basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectAltName=DNS:forgejo.local
EOF
openssl x509 -req -in "$directory/server.csr" -CA "$directory/ca.crt" \
  -CAkey "$directory/ca.key" -CAcreateserial -days 2 \
  -extfile "$directory/server.ext" -out "$directory/server.crt"
printf 'Generated %s; keep ca.key and server.key private.\n' "$directory"
