#!/usr/bin/env bash
# Prepare the load generator for the #98 capacity test: Docker, k6, and a
# Pebble ACME server with its DNS server (pebble-challtestsrv). Run as root on
# a second host in the same private network as the host under test:
#
#   HOST_IP=<host private ip> LOADGEN_IP=<this host's private ip> \
#     scripts/capacity/loadgen-setup.sh
#
# Pebble runs here, not on the host under test, so that the measured host runs
# nothing but PressFleet. Pebble validates HTTP-01 challenges by connecting to
# HOST_IP:80 (the DNS server answers every name with it). Its ports are bound
# to LOADGEN_IP only: the ACME API (14000) for the host's Traefik, and the
# management API (15000) and DNS management API (8055) on loopback.
#
# It writes the CA that signed Pebble's API certificate to
# /root/pebble/ca.pem (Traefik's acme.ca_certificates) and Pebble's issuing
# root, which it generates anew each time it starts, to /root/pebble/roots.pem
# (SSL_CERT_FILE for the control plane's own HTTPS checks of its sites).
# Safe to rerun, but a restart of Pebble invalidates roots.pem.
set -euo pipefail

: "${HOST_IP:?private IP of the host under test}"
: "${LOADGEN_IP:?private IP of this host}"

[[ "$(id -u)" -eq 0 ]] || { echo "run as root" >&2; exit 1; }

# Pinned to the images the end-to-end harness uses (control-plane/internal/e2e/pebble.go).
PEBBLE_IMAGE='ghcr.io/letsencrypt/pebble:2.10.1@sha256:5080492187bea12909c053d7a2b90a9e95104ef1c82381828e7823db71ce7e54'
CHALLTESTSRV_IMAGE='ghcr.io/letsencrypt/pebble-challtestsrv:2.10.1@sha256:1994a326ec60e0de5e8051aa192269c1aaeff0969a4b703d11fa51370f3dc1a8'

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq ca-certificates curl gnupg jq openssl sysstat >/dev/null

if ! docker compose version >/dev/null 2>&1; then
	install -m 0755 -d /etc/apt/keyrings
	curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
	echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
		>/etc/apt/sources.list.d/docker.list
	apt-get update -qq
	apt-get install -y -qq docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin >/dev/null
fi

if ! command -v k6 >/dev/null 2>&1; then
	curl -fsSL https://dl.k6.io/key.gpg | gpg --dearmor -o /etc/apt/keyrings/k6.gpg
	echo "deb [signed-by=/etc/apt/keyrings/k6.gpg] https://dl.k6.io/deb stable main" >/etc/apt/sources.list.d/k6.list
	apt-get update -qq
	apt-get install -y -qq k6 >/dev/null
fi

dir=/root/pebble
mkdir -p "$dir"
cd "$dir"

# A throwaway CA, and a certificate for Pebble's APIs that it signs. The ACME
# API is reached at LOADGEN_IP; the management API on loopback.
if [[ ! -f ca.pem ]]; then
	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 30 \
		-subj '/CN=pf98 pebble API CA' -keyout ca.key -out ca.pem 2>/dev/null
	openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
		-subj '/CN=pebble' -keyout key.pem -out cert.csr 2>/dev/null
	printf 'subjectAltName=DNS:pebble,DNS:localhost,IP:%s,IP:127.0.0.1\n' "$LOADGEN_IP" >san.ext
	openssl x509 -req -in cert.csr -CA ca.pem -CAkey ca.key -CAcreateserial -days 30 \
		-extfile san.ext -out cert.pem 2>/dev/null
fi

cat >config.json <<'EOF'
{"pebble": {
  "listenAddress": "0.0.0.0:14000",
  "managementListenAddress": "0.0.0.0:15000",
  "certificate": "/pebble/cert.pem",
  "privateKey": "/pebble/key.pem",
  "httpPort": 80,
  "tlsPort": 443,
  "ocspResponderURL": "",
  "externalAccountBindingRequired": false,
  "retryAfter": {"authz": 1, "order": 1},
  "keyAlgorithm": "ecdsa",
  "profiles": {"default": {"description": "pf98 capacity test", "validityPeriod": 7776000}}
}}
EOF
chmod 0644 ./*.pem config.json

cat >compose.yaml <<EOF
name: pf98-pebble
services:
  pebble:
    image: "$PEBBLE_IMAGE"
    command: ["-config", "/pebble/config.json", "-dnsserver", "challtestsrv:53"]
    environment:
      PEBBLE_VA_NOSLEEP: "1"
      PEBBLE_WFE_NONCEREJECT: "0"
    volumes:
      - ./:/pebble:ro
    ports:
      - "$LOADGEN_IP:14000:14000"
      - "127.0.0.1:15000:15000"
    depends_on: [challtestsrv]
    restart: unless-stopped
  challtestsrv:
    image: "$CHALLTESTSRV_IMAGE"
    command: ["-defaultIPv6", "", "-dnsserver", ":53", "-http01", "", "-https01", "", "-tlsalpn01", "", "-doh", ""]
    ports:
      - "127.0.0.1:8055:8055"
    restart: unless-stopped
EOF
docker compose up -d

for _ in $(seq 1 30); do
	curl -fsS -X POST -d "{\"ip\":\"$HOST_IP\"}" http://127.0.0.1:8055/set-default-ipv4 && break
	sleep 1
done
for _ in $(seq 1 30); do
	curl -fsS --cacert ca.pem https://localhost:15000/roots/0 -o roots.pem && break
	sleep 1
done
grep -q 'BEGIN CERTIFICATE' roots.pem
echo "Pebble: https://$LOADGEN_IP:14000/dir, every name resolves to $HOST_IP"
k6 version
