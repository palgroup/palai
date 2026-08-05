#!/bin/sh
# Palai device agent installer.
#
#   curl -fsSL https://<host>/install.sh | sh
#   PALAI_VERSION=1.4.2 curl -fsSL https://<host>/install.sh | sh     # pinned, for an image bake
#
# IT INSTALLS THE BINARY AND NOTHING ELSE. It does not enrol, does not ask for a key, does not write or
# load a service. Those belong to `palai enroll`, and keeping them apart is what lets a machine image be
# baked with no secret in it and enrolled later from provider user-data. An installer that also enrolled
# would force every image to carry a credential.
#
# THE WHOLE SCRIPT IS ONE FUNCTION, INVOKED ON THE LAST LINE. A `curl | sh` whose download is truncated
# mid-flight otherwise executes the half that arrived — a real failure mode on a flaky network, and the
# reason `main "$@"` is the final statement rather than a stylistic choice. Everything above it only
# defines functions, so a truncated copy defines some of them and runs none.
#
# THE CHECKSUM DOES NOT COME FROM THE ARCHIVE. The manifest is fetched as its own request and the
# archive is refused on mismatch. A digest served beside the file it protects, by the same credential
# that served the file, protects nothing: whoever can replace one can replace both.
#
# NO PROMPTS AND NO ELEVATION. A provisioner is not a person. If the install prefix is not writable the
# script says which directory and exits non-zero rather than reaching for sudo. `accounts` isolation
# mode's one administrator action stays explicit and separate.
set -eu

PALAI_INSTALL_BASE_URL="${PALAI_INSTALL_BASE_URL:-https://releases.palai.dev}"
PALAI_INSTALL_PREFIX="${PALAI_INSTALL_PREFIX:-$HOME/.local/bin}"
PALAI_VERSION="${PALAI_VERSION:-latest}"

log()  { printf 'palai-install: %s\n' "$*" >&2; }
die()  { printf 'palai-install: %s\n' "$*" >&2; exit 1; }

# need_cmd names the missing tool rather than failing inside a pipeline, where `set -e` would report a
# broken pipe and send the reader looking at the wrong thing.
need_cmd() {
	command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

# detect_triple refuses an unsupported platform BY NAME. Installing a binary that cannot exec produces
# "cannot execute binary file" three steps later, on a machine nobody is logged into.
detect_triple() {
	os_raw="$(uname -s)"
	arch_raw="$(uname -m)"
	case "$os_raw" in
		Darwin) os=darwin ;;
		Linux)  os=linux ;;
		*)      die "unsupported operating system: $os_raw (supported: Darwin, Linux)" ;;
	esac
	case "$arch_raw" in
		arm64|aarch64) arch=arm64 ;;
		x86_64|amd64)  arch=amd64 ;;
		*)             die "unsupported architecture: $arch_raw (supported: arm64, amd64)" ;;
	esac
	printf '%s-%s' "$os" "$arch"
}

fetch() {
	# -f so a 404 is an error rather than an HTML page written to disk, which would then fail the digest
	# check with a message about corruption when the truth is "that version does not exist".
	curl -fsSL "$1" -o "$2" || die "cannot fetch $1"
}

# resolve_version turns "latest" into a concrete version, so what is installed can be REPORTED. A fleet
# of a hundred machines has to be answerable about what it runs without logging into any of them.
resolve_version() {
	if [ "$PALAI_VERSION" != "latest" ]; then
		printf '%s' "$PALAI_VERSION"
		return
	fi
	fetch "$PALAI_INSTALL_BASE_URL/latest.txt" "$work/latest.txt"
	v="$(tr -d ' \t\r\n' < "$work/latest.txt")"
	[ -n "$v" ] || die "the latest-version document is empty at $PALAI_INSTALL_BASE_URL/latest.txt"
	printf '%s' "$v"
}

verify_digest() {
	archive="$1"
	name="$2"
	manifest="$3"
	# The manifest holds "<sha256>  <filename>" lines for every artifact of this release. Select the line
	# by EXACT filename: a substring match would accept the darwin-arm64 digest for a linux-amd64 archive
	# whenever one name is a prefix of the other.
	want="$(awk -v f="$name" '$2 == f { print $1 }' "$manifest")"
	[ -n "$want" ] || die "the checksum manifest names no artifact called $name"
	if command -v sha256sum >/dev/null 2>&1; then
		got="$(sha256sum "$archive" | awk '{print $1}')"
	elif command -v shasum >/dev/null 2>&1; then
		got="$(shasum -a 256 "$archive" | awk '{print $1}')"
	else
		die "required command not found: sha256sum or shasum"
	fi
	[ "$want" = "$got" ] || die "checksum mismatch for $name: manifest says $want, download is $got — refusing to install"
	printf '%s' "$got"
}

# install_binary writes beside the target and renames, so an interrupted install cannot leave a partial
# executable at the name a service manager is about to exec.
install_binary() {
	src="$1"
	dest="$2"
	dir="$(dirname "$dest")"
	mkdir -p "$dir" 2>/dev/null || die "cannot create $dir"
	[ -w "$dir" ] || die "$dir is not writable by $(id -un) — choose another PALAI_INSTALL_PREFIX; this installer does not elevate"
	tmp="$dest.incoming.$$"
	cp "$src" "$tmp" || die "cannot write $tmp"
	chmod 0755 "$tmp"
	mv -f "$tmp" "$dest" || die "cannot install $dest"
}

main() {
	need_cmd curl
	need_cmd tar
	need_cmd awk
	need_cmd uname

	triple="$(detect_triple)"
	work="$(mktemp -d)"
	# shellcheck disable=SC2064
	trap "rm -rf '$work'" EXIT INT TERM

	version="$(resolve_version)"
	name="palai-${version}-${triple}.tar.gz"
	base="$PALAI_INSTALL_BASE_URL/$version"

	log "installing palai $version ($triple) from $base"
	fetch "$base/$name" "$work/$name"
	# Separate request, separate document — see the header.
	fetch "$base/checksums.txt" "$work/checksums.txt"
	digest="$(verify_digest "$work/$name" "$name" "$work/checksums.txt")"

	tar -xzf "$work/$name" -C "$work" || die "cannot extract $name"
	[ -f "$work/palai" ] || die "$name does not contain a palai binary"

	dest="$PALAI_INSTALL_PREFIX/palai"
	# IDEMPOTENT: an identical binary already in place is reported and left alone, so a provisioner that
	# runs this on every boot does not churn the file a running service has open.
	if [ -x "$dest" ] && [ "$("$dest" --version 2>/dev/null || true)" = "$version" ]; then
		log "palai $version is already installed at $dest — nothing to do"
	else
		install_binary "$work/palai" "$dest"
	fi

	# Report what was installed. This is the line a fleet is audited from.
	printf 'palai %s installed\n  path:   %s\n  target: %s\n  sha256: %s\n' \
		"$version" "$dest" "$triple" "$digest"

	case ":$PATH:" in
		*":$PALAI_INSTALL_PREFIX:"*) ;;
		*) log "note: $PALAI_INSTALL_PREFIX is not on PATH" ;;
	esac

	log "next: palai enroll --url <controller> --key-file <path>   (enrol installs and starts the service)"
}

main "$@"
