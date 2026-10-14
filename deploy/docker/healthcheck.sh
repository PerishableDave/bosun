#!/bin/sh
# Docker healthcheck: GET /v1/health on the local daemon, over HTTPS (the
# self-signed certificate is local, so it is not verified) or plain HTTP
# (when bosund runs with behind_proxy and TLS off).
port="${BOSUN_PORT:-7777}"
wget -q -O /dev/null --no-check-certificate "https://127.0.0.1:$port/v1/health" 2>/dev/null ||
	wget -q -O /dev/null "http://127.0.0.1:$port/v1/health"
