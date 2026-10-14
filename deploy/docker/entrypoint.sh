#!/bin/sh
# Container entrypoint for Bosun.
#
# First run: if the config file does not exist, copy the built-in template
# (/etc/bosun/bosun.yaml.template, from deploy/docker/bosun.yaml). If the
# config enables TLS with the generated certificate paths under /data/tls and
# they are missing, create a self-signed certificate. Then exec the command
# (by default: bosund --config /data/bosun.yaml).
set -eu

config="${BOSUN_CONFIG:-/data/bosun.yaml}"
template=/etc/bosun/bosun.yaml.template
tls_dir=/data/tls

log() { printf 'bosun-entrypoint: %s\n' "$*" >&2; }

umask 077

if [ ! -e "$config" ]; then
	mkdir -p "$(dirname "$config")"
	cp "$template" "$config"
	chmod 600 "$config"
	log "created $config from the built-in template; edit it and restart to customise"
	log "create the first admin with: docker compose exec bosun bosun setup --config $config --admin <name>"
fi

# Generate the self-signed certificate only when the config points at the
# default paths (an uncommented "cert_file: /data/tls/cert.pem" line) and
# neither file exists yet; your own certificate files are never touched.
if grep -Eq '^[[:space:]]*cert_file:[[:space:]]*/data/tls/cert\.pem' "$config" &&
	[ ! -e "$tls_dir/cert.pem" ] && [ ! -e "$tls_dir/key.pem" ]; then
	mkdir -p "$tls_dir"
	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
		-days 3650 -subj "/CN=bosun" \
		-addext "subjectAltName=DNS:bosun,DNS:localhost,IP:127.0.0.1,IP:::1" \
		-keyout "$tls_dir/key.pem" -out "$tls_dir/cert.pem" >/dev/null 2>&1
	chmod 600 "$tls_dir/key.pem" "$tls_dir/cert.pem"
	log "generated self-signed TLS certificate in $tls_dir"
fi

exec "$@"
