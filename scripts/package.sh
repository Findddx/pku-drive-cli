#!/bin/sh

set -eu
umask 077
export LC_ALL=C
export TZ=UTC

fail() {
	printf '%s\n' "package: $1" >&2
	exit 1
}

for command_name in awk basename chmod cmp cp date dirname dpkg-deb find git gzip id install jq md5sum mktemp mv realpath rm sha256sum sort stat tar touch xargs; do
	command -v "$command_name" >/dev/null 2>&1 || fail "required command is unavailable: $command_name"
done

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
project_root=$(CDPATH= cd -- "$script_dir/.." && pwd -P)

git_root=$(git -C "$project_root" rev-parse --show-toplevel 2>/dev/null) || fail 'project is not a Git worktree'
git_root=$(CDPATH= cd -- "$git_root" && pwd -P)
if [ "$git_root" != "$project_root" ]; then
	fail 'package script must run from the Git worktree root'
fi
worktree_status=$(git -C "$project_root" status --porcelain=v1 --untracked-files=all) || fail 'cannot inspect Git worktree status'
if [ -n "$worktree_status" ]; then
	fail 'Git worktree must be clean before packaging'
fi

version=${VERSION-}
commit=${COMMIT-}
source_date_epoch=${SOURCE_DATE_EPOCH-}
package_version=${PACKAGE_VERSION:-${version}-1}

case "$version" in
	''|*[!0-9A-Za-z._+-]*) fail 'invalid VERSION' ;;
esac
case "$commit" in
	''|*[!0-9a-f]*) fail 'invalid COMMIT' ;;
esac
commit_length=${#commit}
if [ "$commit_length" -lt 7 ] || [ "$commit_length" -gt 64 ]; then
	fail 'invalid COMMIT'
fi
head_commit=$(git -C "$project_root" rev-parse --short=12 HEAD) || fail 'cannot resolve Git HEAD'
if [ "$commit" != "$head_commit" ]; then
	fail 'COMMIT must exactly match the 12-character Git HEAD identifier'
fi
case "$source_date_epoch" in
	''|*[!0-9]*) fail 'invalid SOURCE_DATE_EPOCH' ;;
esac
head_epoch=$(git -C "$project_root" log -1 --format=%ct) || fail 'cannot resolve Git HEAD timestamp'
if [ "$source_date_epoch" != "$head_epoch" ]; then
	fail 'SOURCE_DATE_EPOCH must exactly match the Git HEAD timestamp'
fi
expected_build_date=$(date -u -d "@$source_date_epoch" '+%Y-%m-%dT%H:%M:%SZ') || fail 'invalid SOURCE_DATE_EPOCH'
case "$package_version" in
	''|*[!0-9A-Za-z.+~:-]*) fail 'invalid PACKAGE_VERSION' ;;
esac
case "$package_version" in
	[0-9]*) ;;
	*) fail 'PACKAGE_VERSION must begin with a digit' ;;
esac

binary=${PACKAGE_BINARY:-$project_root/bin/pku-drive}
case "$binary" in
	/*) ;;
	*) fail 'PACKAGE_BINARY must be an absolute path' ;;
esac
if [ ! -f "$binary" ] || [ -L "$binary" ] || [ ! -x "$binary" ]; then
	fail 'PACKAGE_BINARY must be an executable regular file, not a symbolic link'
fi
binary_real=$(realpath -e -- "$binary") || fail 'cannot resolve PACKAGE_BINARY'
if [ "$binary_real" != "$binary" ]; then
	fail 'PACKAGE_BINARY must be a canonical path without symbolic links'
fi
if ! "$binary" version --json | jq --exit-status \
	--arg version "$version" \
	--arg commit "$commit" \
	--arg build_date "$expected_build_date" \
	'.ok == true and .operation == "version" and .version == $version and .commit == $commit and .build_date == $build_date' \
	>/dev/null; then
	fail 'PACKAGE_BINARY metadata does not match the requested build metadata'
fi

for documentation in \
	"$project_root/README.md" \
	"$project_root/LICENSE" \
	"$project_root/SECURITY.md" \
	"$project_root/CONTRIBUTING.md" \
	"$project_root/docs/api-notes.md" \
	"$project_root/docs/troubleshooting.md"
do
	if [ ! -f "$documentation" ] || [ -L "$documentation" ]; then
		fail 'package documentation must be regular files, not symbolic links'
	fi
	documentation_real=$(realpath -e -- "$documentation") || fail 'cannot resolve package documentation'
	if [ "$documentation_real" != "$documentation" ]; then
		fail 'package documentation paths must be canonical and free of symbolic links'
	fi
done

if [ -n "${PACKAGE_PIN_FILE-}" ]; then
	pin_file=$PACKAGE_PIN_FILE
else
	if [ -n "${XDG_CONFIG_HOME-}" ]; then
		config_home=$XDG_CONFIG_HOME
	else
		case ${HOME-} in
			/*) config_home=$HOME/.config ;;
			*) fail 'HOME must be an absolute path when XDG_CONFIG_HOME is unset' ;;
		esac
	fi
	pin_file=$config_home/pku-drive-cli/object-pin.json
fi
case "$pin_file" in
	/*) ;;
	*) fail 'PACKAGE_PIN_FILE must be an absolute path' ;;
esac
if [ ! -f "$pin_file" ] || [ -L "$pin_file" ]; then
	fail 'object pin input must be a regular file, not a symbolic link'
fi
pin_real=$(realpath -e -- "$pin_file") || fail 'cannot resolve object pin input'
if [ "$pin_real" != "$pin_file" ]; then
	fail 'object pin input must be a canonical path without symbolic links'
fi
pin_dir=$(dirname -- "$pin_file")
if [ ! -d "$pin_dir" ] || [ -L "$pin_dir" ]; then
	fail 'object pin directory must be a directory, not a symbolic link'
fi
pin_dir_real=$(realpath -e -- "$pin_dir") || fail 'cannot resolve object pin directory'
if [ "$pin_dir_real" != "$pin_dir" ]; then
	fail 'object pin directory must be a canonical path without symbolic links'
fi
current_uid=$(id -u)
pin_dir_uid=$(stat -c '%u' -- "$pin_dir") || fail 'cannot inspect object pin directory owner'
pin_dir_mode=$(stat -c '%a' -- "$pin_dir") || fail 'cannot inspect object pin directory mode'
if [ "$pin_dir_uid" != "$current_uid" ] || [ "$pin_dir_mode" != 700 ]; then
	fail 'object pin directory must be owned by the current user with mode 0700'
fi
pin_uid=$(stat -c '%u' -- "$pin_file") || fail 'cannot inspect object pin input owner'
pin_mode=$(stat -c '%a' -- "$pin_file") || fail 'cannot inspect object pin input mode'
pin_links=$(stat -c '%h' -- "$pin_file") || fail 'cannot inspect object pin input link count'
pin_size=$(stat -c '%s' -- "$pin_file") || fail 'cannot inspect object pin input size'
pin_identity=$(stat -c '%d:%i:%s:%Y:%Z' -- "$pin_file") || fail 'cannot inspect object pin input identity'
if [ "$pin_uid" != "$current_uid" ] || [ "$pin_mode" != 600 ] || [ "$pin_links" != 1 ]; then
	fail 'object pin input must be current-user owned, mode 0600, with one hard link'
fi
if [ "$pin_size" -lt 1 ] || [ "$pin_size" -gt 4096 ]; then
	fail 'object pin input has an invalid size'
fi

dist_dir=${DIST_DIR:-$project_root/dist}
case "$dist_dir" in
	/*) ;;
	*) dist_dir=$project_root/$dist_dir ;;
esac
dist_parent=$(dirname -- "$dist_dir")
if [ ! -d "$dist_parent" ] || [ -L "$dist_parent" ]; then
	fail 'distribution parent must be an existing directory, not a symbolic link'
fi
dist_parent_real=$(realpath -e -- "$dist_parent") || fail 'cannot resolve distribution parent'
if [ "$dist_parent_real" != "$dist_parent" ]; then
	fail 'distribution parent must be a canonical path without symbolic links'
fi
if [ -e "$dist_dir" ] || [ -L "$dist_dir" ]; then
	if [ ! -d "$dist_dir" ] || [ -L "$dist_dir" ]; then
		fail 'distribution path must be a directory, not a symbolic link'
	fi
	dist_real=$(realpath -e -- "$dist_dir") || fail 'cannot resolve distribution directory'
	if [ "$dist_real" != "$dist_dir" ]; then
		fail 'distribution directory must be a canonical path without symbolic links'
	fi
else
	install -d -m 0755 -- "$dist_dir"
fi
dist_uid=$(stat -c '%u' -- "$dist_dir") || fail 'cannot inspect distribution directory owner'
dist_mode=$(stat -c '%a' -- "$dist_dir") || fail 'cannot inspect distribution directory mode'
if [ "$dist_uid" != "$current_uid" ] || [ "$dist_mode" != 755 ]; then
	fail 'distribution directory must be current-user owned with mode 0755'
fi

temporary=$(mktemp -d -p "$project_root" .pku-drive-package.XXXXXX) || fail 'cannot create package workspace'
case "$temporary" in
	"$project_root"/.pku-drive-package.*) ;;
	*) fail 'unsafe package workspace path' ;;
esac
if [ ! -d "$temporary" ] || [ -L "$temporary" ]; then
	fail 'unsafe package workspace type'
fi
cleanup() {
	rm -rf -- "$temporary"
}
trap cleanup 0 1 2 15

pin_snapshot=$temporary/object-pin.json
cp --no-dereference -- "$pin_file" "$pin_snapshot" || fail 'cannot snapshot object pin input'
if [ ! -f "$pin_snapshot" ] || [ -L "$pin_snapshot" ]; then
	fail 'object pin snapshot has an unsafe type'
fi
chmod 0600 -- "$pin_snapshot"
if [ "$(stat -c '%d:%i:%s:%Y:%Z' -- "$pin_file")" != "$pin_identity" ] || ! cmp -s -- "$pin_file" "$pin_snapshot"; then
	fail 'object pin input changed while it was being packaged'
fi
if ! jq --stream --slurp --exit-status \
	'length == 2 and
	 .[0][0] == ["spki_sha256"] and
	 (.[0][1] | type == "string" and test("^[0-9a-f]{64}$")) and
	 .[1] == [["spki_sha256"]]' \
	"$pin_snapshot" >/dev/null; then
	fail 'object pin input is not the strict expected JSON object'
fi

input_snapshot=$temporary/inputs
install -d -m 0700 -- "$input_snapshot/docs"
install -m 0755 -- "$binary" "$input_snapshot/pku-drive"
install -m 0644 -- "$project_root/README.md" "$input_snapshot/README.md"
install -m 0644 -- "$project_root/LICENSE" "$input_snapshot/LICENSE"
install -m 0644 -- "$project_root/SECURITY.md" "$input_snapshot/SECURITY.md"
install -m 0644 -- "$project_root/CONTRIBUTING.md" "$input_snapshot/CONTRIBUTING.md"
install -m 0644 -- "$project_root/docs/api-notes.md" "$input_snapshot/docs/api-notes.md"
install -m 0644 -- "$project_root/docs/troubleshooting.md" "$input_snapshot/docs/troubleshooting.md"
if ! cmp -s -- "$binary" "$input_snapshot/pku-drive"; then
	fail 'PACKAGE_BINARY changed while it was being packaged'
fi

install_payload() {
	payload_root=$1
	payload_prefix=$2
	install -d -m 0755 -- \
		"$payload_root/$payload_prefix/bin" \
		"$payload_root/etc/pku-drive-cli" \
		"$payload_root/$payload_prefix/share/doc/pku-drive-cli/docs"
	install -m 0755 -- "$input_snapshot/pku-drive" "$payload_root/$payload_prefix/bin/pku-drive"
	install -m 0644 -- "$pin_snapshot" "$payload_root/etc/pku-drive-cli/object-pin.json"
	install -m 0644 -- "$input_snapshot/README.md" "$payload_root/$payload_prefix/share/doc/pku-drive-cli/README.md"
	install -m 0644 -- "$input_snapshot/LICENSE" "$payload_root/$payload_prefix/share/doc/pku-drive-cli/LICENSE"
	install -m 0644 -- "$input_snapshot/LICENSE" "$payload_root/$payload_prefix/share/doc/pku-drive-cli/copyright"
	install -m 0644 -- "$input_snapshot/SECURITY.md" "$payload_root/$payload_prefix/share/doc/pku-drive-cli/SECURITY.md"
	install -m 0644 -- "$input_snapshot/CONTRIBUTING.md" "$payload_root/$payload_prefix/share/doc/pku-drive-cli/CONTRIBUTING.md"
	install -m 0644 -- "$input_snapshot/docs/api-notes.md" "$payload_root/$payload_prefix/share/doc/pku-drive-cli/docs/api-notes.md"
	install -m 0644 -- "$input_snapshot/docs/troubleshooting.md" "$payload_root/$payload_prefix/share/doc/pku-drive-cli/docs/troubleshooting.md"
}

deb_stage=$temporary/deb
install -d -m 0755 -- "$deb_stage/DEBIAN"
install_payload "$deb_stage" usr
printf '%s\n' \
	'Package: pku-drive-cli' \
	"Version: $package_version" \
	'Section: utils' \
	'Priority: optional' \
	'Architecture: amd64' \
	'Maintainer: Findddx <Findddx@users.noreply.github.com>' \
	'Homepage: https://github.com/Findddx/pku-drive-cli' \
	'Vcs-Git: https://github.com/Findddx/pku-drive-cli.git' \
	'Depends: ca-certificates' \
	'Recommends: xdg-utils' \
	'Description: lightweight command-line client for PKU NetDisk' \
	' A static Linux amd64 command for per-user OAuth login, remote listing,' \
	' directory creation, resumable uploads, personal and shared-link downloads,' \
	' and recycle-bin deletion on the PKU AnyShare deployment.' \
	>"$deb_stage/DEBIAN/control"
printf '%s\n' '/etc/pku-drive-cli/object-pin.json' >"$deb_stage/DEBIAN/conffiles"
(
	cd -- "$deb_stage"
	find etc usr -type f -print0 | LC_ALL=C sort -z | xargs -0 md5sum
) >"$deb_stage/DEBIAN/md5sums"
chmod 0644 -- "$deb_stage/DEBIAN/control" "$deb_stage/DEBIAN/conffiles" "$deb_stage/DEBIAN/md5sums"
find "$deb_stage" -exec touch -h -d "@$source_date_epoch" -- {} +

publish=$temporary/publish
install -d -m 0755 -- "$publish"
deb_name=pku-drive-cli_${package_version}_amd64.deb
SOURCE_DATE_EPOCH=$source_date_epoch dpkg-deb \
	--root-owner-group --uniform-compression --threads-max=1 -Zxz -z9 \
	--build "$deb_stage" "$publish/$deb_name" >/dev/null

bundle_name=pku-drive-cli-${version}-linux-amd64
bundle_root=$temporary/$bundle_name
install -d -m 0755 -- "$bundle_root"
install_payload "$bundle_root" usr/local
tar_name=$bundle_name.tar.gz
tar --sort=name --format=gnu --mtime="@$source_date_epoch" \
	--owner=0 --group=0 --numeric-owner \
	-C "$temporary" -cf - "$bundle_name" | gzip -n -9 >"$publish/$tar_name"

(
	cd -- "$publish"
	sha256sum "$deb_name" "$tar_name" >SHA256SUMS
)
chmod 0644 -- "$publish/$deb_name" "$publish/$tar_name" "$publish/SHA256SUMS"

for artifact in "$deb_name" "$tar_name" SHA256SUMS; do
	target=$dist_dir/$artifact
	if [ -d "$target" ]; then
		fail "refusing to replace directory artifact: $artifact"
	fi
done
for artifact in "$deb_name" "$tar_name" SHA256SUMS; do
	target=$dist_dir/$artifact
	mv -fT -- "$publish/$artifact" "$target"
done

printf '%s\n' \
	"created $dist_dir/$deb_name" \
	"created $dist_dir/$tar_name" \
	"created $dist_dir/SHA256SUMS"
