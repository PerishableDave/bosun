#!/bin/sh
# Bosun installer: downloads a release archive from GitHub, verifies its
# SHA-256 checksum, and installs the bosun and bosund binaries.
#
#   curl -fsSL https://github.com/perishabledave/bosun/releases/latest/download/install.sh | sh
#   curl -fsSL .../install.sh | sh -s -- --user          # into ~/.local/bin, no sudo
#
# Options:
#   --user             install into ~/.local/bin instead of /usr/local/bin
#   --bin-dir DIR      install into DIR
#   --version vX.Y.Z   install that release instead of the latest
#   --base-url URL     download <URL>/bosun_<os>_<arch>.tar.gz and
#                      <URL>/checksums.txt from a mirror (http(s):// or file://)
#   -h, --help         show this help
#
# Supported: macOS and Linux on amd64 (x86_64) and arm64 (aarch64).
# POSIX sh only; needs curl or wget, tar, and sha256sum or shasum.
set -eu

repo="perishabledave/bosun"
bin_dir=""
user_install=0
version=""
base_url=""

say() { printf '%s\n' "$*"; }
err() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

usage() {
	cat <<'EOF'
Usage: install.sh [--user | --bin-dir DIR] [--version vX.Y.Z] [--base-url URL]

  --user             install into ~/.local/bin instead of /usr/local/bin
  --bin-dir DIR      install into DIR
  --version vX.Y.Z   install that release instead of the latest
  --base-url URL     download bosun_<os>_<arch>.tar.gz and checksums.txt
                     from URL (http(s):// or file://) instead of GitHub
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--user) user_install=1 ;;
	--bin-dir) [ $# -ge 2 ] || err "--bin-dir needs a value"; bin_dir="$2"; shift ;;
	--bin-dir=*) bin_dir="${1#*=}" ;;
	--version) [ $# -ge 2 ] || err "--version needs a value"; version="$2"; shift ;;
	--version=*) version="${1#*=}" ;;
	--base-url) [ $# -ge 2 ] || err "--base-url needs a value"; base_url="$2"; shift ;;
	--base-url=*) base_url="${1#*=}" ;;
	-h | --help) usage; exit 0 ;;
	*) err "unknown option: $1 (try --help)" ;;
	esac
	shift
done

# --- platform -------------------------------------------------------------
case "$(uname -s)" in
Darwin) os=darwin ;;
Linux) os=linux ;;
*) err "unsupported OS: $(uname -s) (macOS and Linux only; on Windows use WSL or Docker)" ;;
esac
case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64 | armv8*) arch=arm64 ;;
*) err "unsupported architecture: $(uname -m) (amd64 and arm64 only; 32-bit Raspberry Pi OS needs the 64-bit image)" ;;
esac
# A Rosetta-translated shell on Apple silicon reports x86_64; prefer native.
if [ "$os" = darwin ] && [ "$arch" = amd64 ] &&
	[ "$(sysctl -n sysctl.proc_translated 2>/dev/null || echo 0)" = 1 ]; then
	arch=arm64
fi

# --- where to download from -----------------------------------------------
if [ -z "$base_url" ]; then
	if [ -n "$version" ]; then
		case "$version" in v*) ;; *) version="v$version" ;; esac
		base_url="https://github.com/$repo/releases/download/$version"
	else
		base_url="https://github.com/$repo/releases/latest/download"
	fi
fi
base_url="${base_url%/}"
archive="bosun_${os}_${arch}.tar.gz"

# --- where to install -----------------------------------------------------
if [ -z "$bin_dir" ]; then
	if [ "$user_install" = 1 ]; then
		[ -n "${HOME:-}" ] || err "HOME is not set"
		bin_dir="$HOME/.local/bin"
	else
		bin_dir=/usr/local/bin
	fi
fi

# --- tools ----------------------------------------------------------------
if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL --proto '=https,file,http' -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() {
		case "$1" in
		file://*) cp "${1#file://}" "$2" ;;
		*) wget -q -O "$2" "$1" ;;
		esac
	}
else
	err "need curl or wget"
fi
if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
else
	err "need sha256sum or shasum to verify the download"
fi
command -v tar >/dev/null 2>&1 || err "need tar"

tmp="$(mktemp -d 2>/dev/null || mktemp -d -t bosun-install)"
trap 'rm -rf "$tmp"' EXIT INT TERM

# --- download and verify --------------------------------------------------
say "Downloading $archive from $base_url"
fetch "$base_url/$archive" "$tmp/$archive" || err "download failed: $base_url/$archive"
fetch "$base_url/checksums.txt" "$tmp/checksums.txt" || err "download failed: $base_url/checksums.txt"

want="$(awk -v f="$archive" '$2 == f || $2 == "*" f {print $1; exit}' "$tmp/checksums.txt")"
[ -n "$want" ] || err "$archive is not listed in checksums.txt"
got="$(sha256 "$tmp/$archive")"
[ "$want" = "$got" ] || err "checksum mismatch for $archive (expected $want, got $got)"
say "Checksum OK ($got)"

mkdir "$tmp/x"
tar -xzf "$tmp/$archive" -C "$tmp/x"
for b in bosun bosund; do
	[ -f "$tmp/x/$b" ] || err "$b is missing from $archive"
done

# --- install --------------------------------------------------------------
sudo=""
if [ ! -d "$bin_dir" ]; then
	mkdir -p "$bin_dir" 2>/dev/null || sudo="sudo"
fi
if [ -z "$sudo" ] && [ ! -w "$bin_dir" ]; then
	sudo="sudo"
fi
if [ -n "$sudo" ]; then
	command -v sudo >/dev/null 2>&1 || err "$bin_dir is not writable; re-run with --user or as root"
	say "Installing into $bin_dir (needs sudo; use --user to avoid it)"
	sudo mkdir -p "$bin_dir"
fi
for b in bosun bosund; do
	# Write to a temp name then rename, so a running bosund is not corrupted.
	$sudo cp "$tmp/x/$b" "$bin_dir/.$b.new"
	$sudo chmod 0755 "$bin_dir/.$b.new"
	$sudo mv -f "$bin_dir/.$b.new" "$bin_dir/$b"
done
if [ "$os" = darwin ]; then
	# Downloads by curl are not quarantined, but be safe for file:// mirrors.
	$sudo xattr -d com.apple.quarantine "$bin_dir/bosun" "$bin_dir/bosund" 2>/dev/null || true
fi

installed="$("$bin_dir/bosun" --version 2>/dev/null || echo bosun)"
say "Installed $installed and bosund into $bin_dir"

case ":${PATH:-}:" in
*":$bin_dir:"*) ;;
*)
	say ""
	say "Note: $bin_dir is not on your PATH. Add it, e.g.:"
	say "  echo 'export PATH=\"$bin_dir:\$PATH\"' >> ~/.profile"
	;;
esac

cat <<EOF

Next steps:
  bosun setup              # config, data directory (~/.bosun) and your admin user
  bosun service install    # run bosund in the background at login (coming in this release;
                           # until then: bosund &)
  bosun login && bosun chat "hello"

Docs: https://github.com/$repo/blob/main/docs/DEPLOY.md
EOF
