#!/bin/sh
# Builds the Synology DSM 7 package (.spk) of linux-ha-agent:
#   build-spk.sh BINARY GIT_VERSION GOARCH OUTPUT.spk
# See docs/synology.md for how it runs on DSM.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
binary=$1 gitversion=$2 goarch=$3 out=$4

case $goarch in
amd64) arch=x86_64 ;;
arm64) arch="armv8 aarch64" ;;
*) echo "build-spk.sh: no Synology architecture for $goarch" >&2 && exit 1 ;;
esac

# DSM versions are X.Y.Z-BUILD: releases get build 1, later commits 1000+N,
# so every build sorts after the release it is based on.
v=${gitversion#v}
base=$(printf '%s' "$v" | sed -n 's/^\([0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\).*/\1/p')
commits=$(printf '%s' "$v" | sed -n 's/^[0-9.]*-\([0-9][0-9]*\)-g.*/\1/p')
if [ -z "$base" ]; then
	version=0.0.0-1
elif [ -n "$commits" ]; then
	version=$base-$((1000 + commits))
else
	version=$base-1
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/target/bin" "$work/spk/WIZARD_UIFILES"
install -m 0755 "$binary" "$work/target/bin/linux-ha-agent"
install -m 0755 "$here/target/bin/root-task.sh" "$work/target/bin/root-task.sh"
tar -C "$work/target" --owner=0 --group=0 --numeric-owner -czf "$work/spk/package.tgz" .

cp -R "$here/scripts" "$here/conf" "$work/spk/"
install -m 0755 "$here/WIZARD_UIFILES/upgrade_uifile.sh" "$work/spk/WIZARD_UIFILES/"
SETTINGS=/nonexistent SYNOPKG_TEMP_LOGFILE='' sh "$here/WIZARD_UIFILES/upgrade_uifile.sh" >"$work/spk/WIZARD_UIFILES/install_uifile"
cp "$here/PACKAGE_ICON.PNG" "$here/PACKAGE_ICON_256.PNG" "$work/spk/"
checksum=$(md5sum <"$work/spk/package.tgz" | cut -d' ' -f1)
sed -e "s/@VERSION@/$version/" -e "s/@ARCH@/$arch/" -e "s/@CHECKSUM@/$checksum/" "$here/INFO.in" >"$work/spk/INFO"

tar -C "$work/spk" --owner=0 --group=0 --numeric-owner -cf "$out" \
	INFO package.tgz scripts conf WIZARD_UIFILES PACKAGE_ICON.PNG PACKAGE_ICON_256.PNG
echo "$out: LinuxHAAgent $version ($arch)"
