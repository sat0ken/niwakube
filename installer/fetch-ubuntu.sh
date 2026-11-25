#!/usr/bin/env bash
# Ubuntu の mini ISO を取得し、インストール時にダウンロードする Server ISO の情報を調べる。
# どちらも SHA256SUMS の GPG 署名 (Ubuntu CD Image Automatic Signing Key) を検証する。
#
#   ./installer/fetch-ubuntu.sh 26.04 out/
#     → out/mini.iso       検証済みの mini ISO
#       out/server.env     SERVER_ISO_VERSION / URL / SIZE / SHA256 (最新のポイントリリース)
set -euo pipefail

RELEASE=${1:?usage: $0 RELEASE OUTDIR}
OUT=${2:?usage: $0 RELEASE OUTDIR}
# Ubuntu CD Image Automatic Signing Key (2012) <cdimage@ubuntu.com>
KEY_FPR=843938DF228D22F7B3742BC0D94AA3F0EFE21092
MINI_BASE=https://cdimage.ubuntu.com/ubuntu-mini-iso/releases/$RELEASE/release
SERVER_BASE=https://releases.ubuntu.com/$RELEASE

log() { printf '\e[1;34m==>\e[0m %s\n' "$*"; }
die() { printf '\e[1;31mxx\e[0m %s\n' "$*" >&2; exit 1; }

mkdir -p "$OUT"
GNUPGHOME=$(mktemp -d)
export GNUPGHOME
trap 'rm -rf "$GNUPGHOME"' EXIT

log "署名鍵の取得 ($KEY_FPR)"
# 鍵サーバーから HTTPS で取得する (署名の検証では VALIDSIG の指紋が KEY_FPR と一致することを確かめる)
curl -fsSL "https://keyserver.ubuntu.com/pks/lookup?op=get&options=mr&search=0x$KEY_FPR" | gpg --batch --quiet --import

verify_sums() { # DIR URL_BASE
  curl -fsSL -o "$1/SHA256SUMS" "$2/SHA256SUMS"
  curl -fsSL -o "$1/SHA256SUMS.gpg" "$2/SHA256SUMS.gpg"
  gpg --batch --status-fd 1 --verify "$1/SHA256SUMS.gpg" "$1/SHA256SUMS" 2>/dev/null \
    | grep -q "^\[GNUPG:\] VALIDSIG $KEY_FPR" || die "$2/SHA256SUMS の署名を検証できませんでした"
}

# 最新のポイントリリースのファイル名を選ぶ (例: 26.04 と 26.04.1 があれば 26.04.1)
latest() { # SUMS PATTERN
  grep -E "$2" "$1" | sed 's/^[0-9a-f]* \*//' | sort -V | tail -n 1
}

log "mini ISO ($MINI_BASE)"
mkdir -p "$OUT/mini"
verify_sums "$OUT/mini" "$MINI_BASE"
mini=$(latest "$OUT/mini/SHA256SUMS" "ubuntu-mini-iso-$RELEASE(\.[0-9]+)?-mini-iso-amd64\.iso$")
[[ -n $mini ]] || die "mini ISO が見つかりません"
if [[ ! -f $OUT/mini.iso ]] || ! (cd "$OUT/mini" && ln -sf ../mini.iso "$mini" && sha256sum --quiet -c --ignore-missing SHA256SUMS); then
  curl -fsSL -o "$OUT/mini.iso" "$MINI_BASE/$mini"
fi
(cd "$OUT/mini" && ln -sf ../mini.iso "$mini" && sha256sum -c --ignore-missing SHA256SUMS) || die "mini ISO のチェックサムが一致しません"

log "Server ISO ($SERVER_BASE)"
mkdir -p "$OUT/server"
verify_sums "$OUT/server" "$SERVER_BASE"
server=$(latest "$OUT/server/SHA256SUMS" "ubuntu-$RELEASE(\.[0-9]+)?-live-server-amd64\.iso$")
[[ -n $server ]] || die "Server ISO が見つかりません"
sum=$(grep -E " \*?$server\$" "$OUT/server/SHA256SUMS" | cut -d' ' -f1)
size=$(curl -fsSIL "$SERVER_BASE/$server" | tr -d '\r' | awk 'tolower($1)=="content-length:" {n=$2} END {print n}')
[[ $size =~ ^[0-9]+$ ]] || die "Server ISO のサイズを取得できませんでした"
version=${server#ubuntu-}
version=${version%-live-server-amd64.iso}

cat >"$OUT/server.env" <<EOF
SERVER_ISO_VERSION=$version
SERVER_ISO_URL=$SERVER_BASE/$server
SERVER_ISO_SIZE=$size
SERVER_ISO_SHA256=$sum
EOF
cat "$OUT/server.env"
