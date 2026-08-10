#!/usr/bin/env bash
set -euo pipefail
umask 077

pku_fail() {
    printf 'live acceptance: %s\n' "$*" >&2
    exit 1
}

test "${PKU_DRIVE_LIVE:-}" = 1 || pku_fail 'PKU_DRIVE_LIVE=1 is required to run live acceptance'

pku_script_source=${BASH_SOURCE[0]}
test ! -L "$pku_script_source" || pku_fail 'refusing a symlinked acceptance script'
pku_script_dir=$(cd -P -- "$(dirname -- "$pku_script_source")" && pwd)
pku_repo=$(cd -P -- "$pku_script_dir/.." && pwd)
cd -- "$pku_repo"
test "$(git rev-parse --show-toplevel)" = "$pku_repo" || pku_fail 'script is not inside the expected git worktree'
test "$(awk '$1 == "module" { print $2; exit }' go.mod)" = pku-drive-cli || pku_fail 'unexpected Go module'

pku_start_commit=$(git rev-parse HEAD) || pku_fail 'could not resolve the tested commit'
[[ $pku_start_commit =~ ^[0-9a-f]{40}$ ]] || pku_fail 'unexpected tested commit identity'
pku_start_short=${pku_start_commit:0:12}
if ! pku_git_status=$(git status --porcelain=v1 --untracked-files=all); then
    pku_fail 'could not verify the worktree state'
fi
test -z "$pku_git_status" || pku_fail 'live acceptance requires a clean committed worktree'

case ${HOME-} in /*) ;; *) pku_fail 'HOME must be an absolute path' ;; esac
pku_home=$(realpath -ms -- "$HOME")
test "$pku_home" != / && test "$pku_home" = "$HOME" || pku_fail 'HOME must be canonical and non-root'
pku_config_home=${XDG_CONFIG_HOME:-"$pku_home/.config"}
pku_state_home=${XDG_STATE_HOME:-"$pku_home/.local/state"}
pku_canonical_config_home=$(realpath -ms -- "$pku_config_home")
pku_canonical_state_home=$(realpath -ms -- "$pku_state_home")
test "$pku_canonical_config_home" != / && test "$pku_canonical_config_home" = "$pku_config_home" || \
    pku_fail 'XDG_CONFIG_HOME must be canonical, absolute, and non-root'
test "$pku_canonical_state_home" != / && test "$pku_canonical_state_home" = "$pku_state_home" || \
    pku_fail 'XDG_STATE_HOME must be canonical, absolute, and non-root'

pku_cli="$pku_home/.local/bin/pku-drive"
pku_credentials="$pku_config_home/pku-drive-cli/credentials.json"
pku_object_pin="$pku_config_home/pku-drive-cli/object-pin.json"
pku_state_dir="$pku_state_home/pku-drive-cli/uploads"
pku_live_tmp=$(mktemp -d -p /tmp pku-drive-live.XXXXXXXX)
chmod 700 "$pku_live_tmp"

pku_live_nonce=''
pku_library_path=''
pku_library_id=''
pku_live_name=''
pku_live_dir=''
pku_live_dir_id=''
pku_manifest="$pku_live_tmp/live-manifest.json"
pku_small_local=''
pku_small_remote=''
pku_small_size=0
pku_small_initial_size=0
pku_small_id=''
pku_large_local=''
pku_large_remote=''
pku_large_size=134217728
pku_large_id=''
pku_upload_pid=''
pku_cleaned=false
pku_mutation_attempted=false
pku_resume_covered=false

pku_stop_background_upload() {
    local pku_pid=${pku_upload_pid:-}
    test -n "$pku_pid" || return 0
    if kill -0 "$pku_pid" 2>/dev/null; then
        kill -INT "$pku_pid" 2>/dev/null || true
    fi
    set +e
    wait "$pku_pid" 2>/dev/null
    set -e
    pku_upload_pid=''
}

pku_on_exit() {
    local pku_rc=$?
    trap - EXIT INT TERM HUP
    pku_stop_background_upload
    if test "$pku_rc" -ne 0 && test "$pku_cleaned" != true && test "$pku_mutation_attempted" = true; then
        printf 'LIVE CLEANUP REQUIRED: name=%s manifest=%s\n' "${pku_live_name:-unknown}" "$pku_manifest" >&2
    fi
    exit "$pku_rc"
}
trap pku_on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

pku_write_manifest() {
    local pku_manifest_tmp
    pku_manifest_tmp=$(mktemp "$pku_live_tmp/.live-manifest.XXXXXXXX")
    if ! jq -n \
        --arg nonce "$pku_live_nonce" \
        --arg library_path "$pku_library_path" \
        --arg library_id "$pku_library_id" \
        --arg test_path "$pku_live_dir" \
        --arg test_id "$pku_live_dir_id" \
        --argjson small_size "$pku_small_size" \
        --arg small_id "$pku_small_id" \
        --argjson large_size "$pku_large_size" \
        --arg large_id "$pku_large_id" \
        '{nonce:$nonce,library_path:$library_path,library_id:$library_id,test_path:$test_path,test_id:$test_id,small_size:$small_size,small_id:$small_id,large_size:$large_size,large_id:$large_id}' \
        >"$pku_manifest_tmp"; then
        rm -f -- "$pku_manifest_tmp"
        return 1
    fi
    chmod 600 "$pku_manifest_tmp"
    mv -fT -- "$pku_manifest_tmp" "$pku_manifest"
    test ! -L "$pku_manifest"
    test "$(stat -c '%a' "$pku_manifest")" = 600
    test "$(stat -c '%u' "$pku_manifest")" = "$(id -u)"
}

pku_record_completed_large_upload() {
    local pku_json_file=$1
    jq -e --arg p "$pku_large_remote" --argjson s "$pku_large_size" \
        '.ok == true and .remote_path == $p and .size == $s and (.remote_id | startswith("gns://"))' \
        "$pku_json_file" >/dev/null || return 1
    pku_large_id=$(jq -er '.remote_id' "$pku_json_file")
    pku_write_manifest
}

pku_state_matches=()
pku_find_resume_states() {
    local pku_candidate pku_base
    pku_state_matches=()
    test -d "$pku_state_dir" || return 0
    shopt -s nullglob
    for pku_candidate in "$pku_state_dir"/*.json; do
        test -f "$pku_candidate" && test ! -L "$pku_candidate" || continue
        pku_base=${pku_candidate##*/}
        [[ $pku_base =~ ^[0-9a-f]{64}\.json$ ]] || continue
        test "$(stat -c '%u' "$pku_candidate")" = "$(id -u)" || continue
        test "$(stat -c '%a' "$pku_candidate")" = 600 || continue
        if jq -e \
            --arg server 'https://disk.pku.edu.cn' \
            --arg remote "$pku_large_remote" \
            --arg local "$pku_large_local" \
            --argjson size "$pku_large_size" \
            '.server == $server and .remote_path == $remote and .local_path == $local and
             .fingerprint.size == $size and .phase == "uploading" and
             (.doc_id | startswith("gns://")) and (.revision | length > 0) and
             (.upload_id | length > 0) and (.part_size > 0) and
             (.completed | type == "object") and ((.completed | length) >= 1)' \
            "$pku_candidate" >/dev/null 2>&1; then
            pku_state_matches+=("$pku_candidate")
        fi
    done
    shopt -u nullglob
}

command -v jq >/dev/null
command -v go >/dev/null
command -v make >/dev/null
command -v realpath >/dev/null

# The deployed object endpoint does not build to a public CA root. Require the
# separately enrolled, exact PKU pin before any remote mutation; never derive
# or print it from this destructive acceptance script.
test -z "${PKU_DRIVE_OBJECT_SPKI_SHA256:-}" || \
    pku_fail 'unset PKU_DRIVE_OBJECT_SPKI_SHA256 so acceptance exercises the canonical pin file'
test -d "$(dirname -- "$pku_object_pin")" && test ! -L "$(dirname -- "$pku_object_pin")" || \
    pku_fail 'private PKU config directory is missing or unsafe'
test "$(stat -c '%u' "$(dirname -- "$pku_object_pin")")" = "$(id -u)" || pku_fail 'PKU config directory owner is unsafe'
test "$(stat -c '%a' "$(dirname -- "$pku_object_pin")")" = 700 || pku_fail 'PKU config directory mode must be 0700'
test -f "$pku_object_pin" && test ! -L "$pku_object_pin" || pku_fail 'canonical PKU object pin is missing or unsafe'
test "$(stat -c '%u' "$pku_object_pin")" = "$(id -u)" || pku_fail 'PKU object pin owner is unsafe'
test "$(stat -c '%a' "$pku_object_pin")" = 600 || pku_fail 'PKU object pin mode must be 0600'
jq -e 'type == "object" and keys == ["spki_sha256"] and
       (.spki_sha256 | type == "string" and test("^[0-9a-f]{64}$"))' "$pku_object_pin" >/dev/null || \
    pku_fail 'PKU object pin schema is invalid'

make install
test -f "$pku_cli" && test ! -L "$pku_cli"
test "$(stat -c '%u' "$pku_cli")" = "$(id -u)"
test "$(stat -c '%a' "$pku_cli")" = 755
GOENV=off GOFLAGS= GOWORK=off GOTOOLCHAIN=local go version -m "$pku_cli" | \
    awk '$1 == "path" && $2 == "github.com/Findddx/pku-drive-cli/cmd/pku-drive" { found=1 } END { exit !found }'
"$pku_cli" version --json | jq -e --arg commit "$pku_start_short" \
    '.ok == true and .operation == "version" and .commit == $commit' >/dev/null

printf '%s\n' 'OAuth will use the displayed server-side browser. A browser on another host requires an exact-port SSH loopback tunnel.' >&2
"$pku_cli" login
test -f "$pku_credentials" && test ! -L "$pku_credentials"
test "$(stat -c '%u' "$pku_credentials")" = "$(id -u)"
test "$(stat -c '%a' "$pku_credentials")" = 600
test "$(stat -c '%a' "$(dirname -- "$pku_credentials")")" = 700

pku_status_json=$("$pku_cli" status --json)
printf '%s\n' "$pku_status_json" | jq -e '.ok == true and .logged_in == true and .server == "https://disk.pku.edu.cn"' >/dev/null
pku_root_json=$("$pku_cli" ls / --json)
printf '%s\n' "$pku_root_json" | jq -e '.ok == true and (.entries | type == "array")' >/dev/null

if test -n "${PKU_DRIVE_LIVE_LIBRARY_PATH:-}"; then
    pku_library_candidates=$(printf '%s\n' "$pku_root_json" | jq -c --arg p "$PKU_DRIVE_LIVE_LIBRARY_PATH" \
        '[.entries[] | select(.type == "user_doc_lib" and .remote_path == $p)]')
else
    pku_library_candidates=$(printf '%s\n' "$pku_root_json" | jq -c '[.entries[] | select(.type == "user_doc_lib")]')
fi
test "$(printf '%s\n' "$pku_library_candidates" | jq -er 'length')" -eq 1 || \
    pku_fail 'expected exactly one selected user_doc_lib; set PKU_DRIVE_LIVE_LIBRARY_PATH to an exact listed path'
pku_library_path=$(printf '%s\n' "$pku_library_candidates" | jq -er '.[0].remote_path')
pku_library_id=$(printf '%s\n' "$pku_library_candidates" | jq -er '.[0].remote_id | select(startswith("gns://"))')
[[ $pku_library_path =~ ^/[^/]+$ ]] || pku_fail 'selected library path is not one visible root segment'

# Prove that the server's current limits make the approved random 128 MiB file
# multipart before creating any remote object.
PKU_DRIVE_LIVE=1 GOENV=off GOFLAGS= GOWORK=off GOTOOLCHAIN=local \
    go test -count=1 -tags=live ./internal/live -run '^TestServerPlansMultipartFor128MiB$' -v

pku_live_nonce=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
[[ $pku_live_nonce =~ ^[0-9a-f]{32}$ ]] || pku_fail 'failed to create a 128-bit lowercase nonce'
pku_live_name="codex-smoke-$pku_live_nonce"
pku_live_dir="$pku_library_path/$pku_live_name"
pku_small_local="$pku_live_tmp/small.txt"
pku_small_remote="$pku_live_dir/小文件.txt"
pku_large_local="$pku_live_tmp/multipart-128MiB.bin"
pku_large_remote="$pku_live_dir/multipart-128MiB.bin"
pku_conflict_json="$pku_live_tmp/conflict.json"
pku_first_json="$pku_live_tmp/multipart-first.json"
pku_first_stderr="$pku_live_tmp/multipart-first.stderr"

printf 'pku-drive live smoke %s\n' "$pku_live_nonce" >"$pku_small_local"
pku_small_size=$(stat -c '%s' "$pku_small_local")
pku_small_initial_size=$pku_small_size
dd if=/dev/urandom of="$pku_large_local" bs=1048576 count=128 status=none
test "$(stat -c '%s' "$pku_large_local")" -eq "$pku_large_size"

pku_library_before=$("$pku_cli" ls "$pku_library_path" --json)
printf '%s\n' "$pku_library_before" | jq -e --arg n "$pku_live_name" --arg p "$pku_live_dir" \
    '[.entries[] | select(.name == $n or .remote_path == $p)] | length == 0' >/dev/null || \
    pku_fail 'nonce-bound test directory already exists; refusing to adopt it'

pku_mutation_attempted=true
pku_mkdir_json=$("$pku_cli" mkdir "$pku_live_dir" --json)
pku_live_dir_id=$(printf '%s\n' "$pku_mkdir_json" | jq -er --arg n "$pku_live_name" --arg p "$pku_live_dir" \
    'select(.ok == true and .name == $n and .remote_path == $p and .type == "directory") | .remote_id | select(startswith("gns://"))')
pku_write_manifest
pku_library_after_mkdir=$("$pku_cli" ls "$pku_library_path" --json)
printf '%s\n' "$pku_library_after_mkdir" | jq -e --arg n "$pku_live_name" --arg p "$pku_live_dir" --arg id "$pku_live_dir_id" \
    '[.entries[] | select(.name == $n and .remote_path == $p and .remote_id == $id and .type == "directory")] | length == 1' >/dev/null

pku_small_put_json=$("$pku_cli" put "$pku_small_local" "$pku_small_remote" --json)
printf '%s\n' "$pku_small_put_json" | jq -e --arg p "$pku_small_remote" --argjson s "$pku_small_size" \
    '.ok == true and .remote_path == $p and .size == $s and (.remote_id | startswith("gns://"))' >/dev/null
pku_small_id=$(printf '%s\n' "$pku_small_put_json" | jq -er '.remote_id')
pku_write_manifest
pku_small_ls_json=$("$pku_cli" ls "$pku_live_dir" --json)
printf '%s\n' "$pku_small_ls_json" | jq -e --arg n '小文件.txt' --arg id "$pku_small_id" --argjson s "$pku_small_size" \
    '[.entries[] | select(.name == $n and .remote_id == $id and .type == "file" and .size == $s)] | length == 1' >/dev/null

set +e
"$pku_cli" put "$pku_small_local" "$pku_small_remote" --json >"$pku_conflict_json"
pku_conflict_rc=$?
set -e
test "$pku_conflict_rc" -eq 4
jq -e '.ok == false and .category == "remote"' "$pku_conflict_json" >/dev/null

printf 'pku-drive live overwritten %s\n' "$pku_live_nonce" >"$pku_small_local"
pku_small_size=$(stat -c '%s' "$pku_small_local")
pku_overwrite_json=$("$pku_cli" put "$pku_small_local" "$pku_small_remote" --overwrite --json)
printf '%s\n' "$pku_overwrite_json" | jq -e --arg p "$pku_small_remote" --arg id "$pku_small_id" --argjson s "$pku_small_size" \
    '.ok == true and .remote_path == $p and .remote_id == $id and .size == $s' >/dev/null
pku_write_manifest
pku_small_ls_json=$("$pku_cli" ls "$pku_live_dir" --json)
printf '%s\n' "$pku_small_ls_json" | jq -e --arg n '小文件.txt' --arg id "$pku_small_id" --argjson s "$pku_small_size" \
    '[.entries[] | select(.name == $n and .remote_id == $id and .type == "file" and .size == $s)] | length == 1' >/dev/null

"$pku_cli" put "$pku_large_local" "$pku_large_remote" --json --quiet >"$pku_first_json" 2>"$pku_first_stderr" &
pku_upload_pid=$!
pku_resume_state=''
for ((pku_try=0; pku_try<600; pku_try++)); do
    pku_find_resume_states
    if test "${#pku_state_matches[@]}" -gt 1; then
        pku_fail 'multiple matching resume states found'
    fi
    if test "${#pku_state_matches[@]}" -eq 1; then
        pku_resume_state=${pku_state_matches[0]}
        break
    fi
    kill -0 "$pku_upload_pid" 2>/dev/null || break
    sleep 0.05
done

pku_completed_without_interrupt=false
pku_part_size=0
pku_completed_before=0
pku_parts_total=0
pku_parts_resumed=0
pku_parts_uploaded=0

if test -z "$pku_resume_state"; then
    set +e
    wait "$pku_upload_pid"
    pku_first_rc=$?
    set -e
    pku_upload_pid=''
    test "$pku_first_rc" -eq 0 || pku_fail "128 MiB upload produced no resumable state (rc=$pku_first_rc)"
    pku_record_completed_large_upload "$pku_first_json" || pku_fail 'completed upload returned invalid JSON'
    pku_completed_without_interrupt=true
else
    set +e
    kill -INT "$pku_upload_pid" 2>/dev/null
    wait "$pku_upload_pid"
    pku_first_rc=$?
    set -e
    pku_upload_pid=''
    if test "$pku_first_rc" -eq 0; then
        pku_record_completed_large_upload "$pku_first_json" || pku_fail 'completed upload returned invalid JSON'
        pku_completed_without_interrupt=true
    else
        test "$pku_first_rc" -eq 130 || pku_fail "interrupted upload exited $pku_first_rc instead of 130"
    fi
fi

if test "$pku_completed_without_interrupt" = true; then
    jq -e --arg p "$pku_large_remote" --argjson s "$pku_large_size" \
        '.ok == true and .remote_path == $p and .size == $s and .multipart == true and
         (.remote_id | startswith("gns://")) and (.parts_total > 1) and
         (.parts_resumed + .parts_uploaded == .parts_total)' "$pku_first_json" >/dev/null
    pku_parts_total=$(jq -er '.parts_total' "$pku_first_json")
    pku_parts_resumed=$(jq -er '.parts_resumed' "$pku_first_json")
    pku_parts_uploaded=$(jq -er '.parts_uploaded' "$pku_first_json")
else
    test -f "$pku_resume_state" && test ! -L "$pku_resume_state"
    test "$(stat -c '%u' "$pku_state_dir")" = "$(id -u)"
    test "$(stat -c '%a' "$pku_state_dir")" = 700
    test "$(stat -c '%u' "$pku_resume_state")" = "$(id -u)"
    test "$(stat -c '%a' "$pku_resume_state")" = 600
    jq -e --arg remote "$pku_large_remote" --arg local "$pku_large_local" --argjson size "$pku_large_size" \
        '.server == "https://disk.pku.edu.cn" and .remote_path == $remote and .local_path == $local and
         .fingerprint.size == $size and .phase == "uploading" and (.doc_id | startswith("gns://")) and
         (.revision | length > 0) and (.upload_id | length > 0) and (.part_size > 0) and
         (.completed | type == "object") and ((.completed | length) >= 1)' "$pku_resume_state" >/dev/null
    pku_part_size=$(jq -er '.part_size' "$pku_resume_state")
    pku_completed_before=$(jq -er '.completed | length' "$pku_resume_state")
    pku_parts_total=$(( (pku_large_size + pku_part_size - 1) / pku_part_size ))
    test "$pku_parts_total" -gt 1
    test "$pku_completed_before" -ge 1
    test "$pku_completed_before" -le "$pku_parts_total"

    pku_resume_json=$("$pku_cli" put "$pku_large_local" "$pku_large_remote" --json --quiet)
    printf '%s\n' "$pku_resume_json" | jq -e \
        --arg p "$pku_large_remote" --argjson s "$pku_large_size" --argjson saved "$pku_completed_before" \
        '.ok == true and .remote_path == $p and .size == $s and (.remote_id | startswith("gns://")) and
         .multipart == true and .resumed == true and .parts_resumed == $saved and
         (.parts_resumed + .parts_uploaded == .parts_total) and (.parts_uploaded < .parts_total)' >/dev/null
    pku_large_id=$(printf '%s\n' "$pku_resume_json" | jq -er '.remote_id')
    pku_parts_resumed=$(printf '%s\n' "$pku_resume_json" | jq -er '.parts_resumed')
    pku_parts_uploaded=$(printf '%s\n' "$pku_resume_json" | jq -er '.parts_uploaded')
    pku_result_parts_total=$(printf '%s\n' "$pku_resume_json" | jq -er '.parts_total')
    test "$pku_result_parts_total" -eq "$pku_parts_total"
    pku_write_manifest
    test ! -e "$pku_resume_state"
    pku_resume_covered=true
fi

pku_final_dir_json=$("$pku_cli" ls "$pku_live_dir" --json)
printf '%s\n' "$pku_final_dir_json" | jq -e \
    --arg sid "$pku_small_id" --arg lid "$pku_large_id" --argjson ss "$pku_small_size" --argjson ls "$pku_large_size" \
    '(.entries | length) == 2 and
     ([.entries[] | select(.name == "小文件.txt" and .remote_id == $sid and .type == "file" and .size == $ss)] | length) == 1 and
     ([.entries[] | select(.name == "multipart-128MiB.bin" and .remote_id == $lid and .type == "file" and .size == $ls)] | length) == 1' >/dev/null

PKU_DRIVE_LIVE=1 PKU_DRIVE_LIVE_MANIFEST="$pku_manifest" \
    GOENV=off GOFLAGS= GOWORK=off GOTOOLCHAIN=local \
    go test -count=1 -tags=live ./internal/live -run '^TestCleanupExactDirectory$' -v
pku_library_final=$("$pku_cli" ls "$pku_library_path" --json)
printf '%s\n' "$pku_library_final" | jq -e --arg n "$pku_live_name" --arg id "$pku_live_dir_id" --arg p "$pku_live_dir" \
    '[.entries[] | select(.name == $n or .remote_id == $id or .remote_path == $p)] | length == 0' >/dev/null
test "$(stat -c '%u' "$pku_credentials")" = "$(id -u)"
test "$(stat -c '%a' "$pku_credentials")" = 600
pku_cleaned=true

rm -f -- "$pku_small_local" "$pku_large_local" "$pku_conflict_json" "$pku_first_json" "$pku_first_stderr" "$pku_manifest"
rmdir -- "$pku_live_tmp"

test "$(git rev-parse HEAD)" = "$pku_start_commit" || pku_fail 'HEAD changed during live acceptance'
if ! pku_final_status=$(git status --porcelain=v1 --untracked-files=all); then
    pku_fail 'could not verify final worktree state'
fi
test -z "$pku_final_status" || pku_fail 'worktree changed during live acceptance'
jq -nc \
    --arg tested_commit "$pku_start_short" \
    --arg test_name "$pku_live_name" \
    --argjson small_initial_bytes "$pku_small_initial_size" \
    --argjson small_overwrite_bytes "$pku_small_size" \
    --argjson multipart_bytes "$pku_large_size" \
    --argjson part_size "$pku_part_size" \
    --argjson saved "$pku_completed_before" \
    --argjson total "$pku_parts_total" \
    --argjson resumed "$pku_parts_resumed" \
    --argjson uploaded "$pku_parts_uploaded" \
    --argjson resume_covered "$pku_resume_covered" \
    '{live_result:true,tested_commit:$tested_commit,server:"https://disk.pku.edu.cn",test_name:$test_name,small_initial_bytes:$small_initial_bytes,small_overwrite_bytes:$small_overwrite_bytes,multipart_bytes:$multipart_bytes,part_size:(if $resume_covered then $part_size else null end),parts_saved_before_interrupt:$saved,parts_total:$total,parts_resumed:$resumed,parts_uploaded:$uploaded,resume_covered:$resume_covered,cleanup:true}'
