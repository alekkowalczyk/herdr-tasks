#!/bin/sh
# Puts herdr-tasks in bin/. With Go installed it builds this checkout;
# otherwise it downloads the release binary for this version and checks it
# against the release's checksums.
set -eu
mkdir -p bin

if command -v go >/dev/null 2>&1; then
  exec go build -o bin/herdr-tasks ./cmd/herdr-tasks
fi

fail() { echo "herdr-tasks: $*" >&2; exit 1; }

version=$(sed -n 's/^version = "\(.*\)"$/\1/p' herdr-plugin.toml)
case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) fail "no prebuilt binary for $(uname -s); install Go and try again" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) fail "no prebuilt binary for $(uname -m); install Go and try again" ;;
esac

name="herdr-tasks-$os-$arch"
base="https://github.com/alekkowalczyk/herdr-tasks/releases/download/v$version"
fetch() {
  if command -v curl >/dev/null 2>&1; then curl -fsSL "$1" -o "$2"
  elif command -v wget >/dev/null 2>&1; then wget -qO "$2" "$1"
  else fail "needs curl or wget to download $1 (or install Go)"
  fi
}
fetch "$base/$name" bin/herdr-tasks.download || fail "couldn't download $base/$name"
fetch "$base/checksums.txt" bin/checksums.txt || fail "couldn't download $base/checksums.txt"

want=$(grep " $name\$" bin/checksums.txt | cut -d' ' -f1)
if command -v sha256sum >/dev/null 2>&1; then
  got=$(sha256sum bin/herdr-tasks.download | cut -d' ' -f1)
else
  got=$(shasum -a 256 bin/herdr-tasks.download | cut -d' ' -f1)
fi
rm -f bin/checksums.txt
if [ -z "$want" ] || [ "$want" != "$got" ]; then
  rm -f bin/herdr-tasks.download
  fail "the downloaded $name doesn't match the release checksum"
fi
chmod +x bin/herdr-tasks.download
mv bin/herdr-tasks.download bin/herdr-tasks
echo "herdr-tasks: downloaded $name v$version"
