#!/usr/bin/env bash
set -e
# All built binaries live in /app; add to PATH so compose commands can be bare names.
export PATH="/app:${PATH}"

# Beta runs every service with WARP_ENV=local. Since the 2026-10-18 upstream
# merge every WARP_ENV=local process refuses each PostgreSQL and Redis dial
# unless WARP_TEST_ENV_ATTESTED_ADDRS lists the ip:port it resolves to
# (test_env_dial_guard.go). Upstream's launcher scripts (test-env.sh,
# local/local-env.sh) export that attestation; beta has no launcher, so this
# entrypoint attests the endpoints the process will dial before it starts:
#   - the authority of vault pg.yml, of pg_maintenance.yml when it exists (the
#     maintenance pool and read-only processes dial it), and of redis.yml, read
#     from the first vault directory that holds each, in the Go resolver's order
#     (root, root/$WARP_ENV, root/all)
#   - each authority host resolved once with getent ahosts, which reads the same
#     /etc/hosts and Docker DNS the guard's resolver does
# A value that is already set is left alone. Anything that would leave the
# process unable to dial (no vault file, a name that does not resolve, a
# loopback or IPv6 answer, a Redis cluster, more than 8 endpoints) stops the
# container here with a message instead of starting a process that refuses
# every dial.
#
# The attestation is fixed for the life of the process. If postgres or redis is
# recreated and comes back on another address, the running services refuse
# every reconnect until they are restarted, which runs this again. A
# `docker exec <container> bringyourctl ...` does not pass through this
# entrypoint and gets no attestation, so it is refused; run it as
# `docker exec <container> /app/beta-entrypoint.sh bringyourctl ...`.

beta_attest_error() {
    echo "[beta-entrypoint]local dial attestation: $*" >&2
    exit 1
}

# Mirrors testEnvLocalEnvironment: WARP_ENV=local in any case and with
# surrounding space, or no WARP_ENV and a vault home whose own name is local.
beta_local_environment() {
    local warp_env="$1"
    local vault_home="${WARP_VAULT_HOME:-}"
    if [[ "${warp_env,,}" == local ]]; then
        return 0
    fi
    [[ -z "$warp_env" && -n "$vault_home" ]] || return 1
    vault_home="${vault_home%/}"
    [[ "${vault_home##*/}" =~ ^[Ll][Oo][Cc][Aa][Ll]$ ]]
}

# Prints the path of the first vault directory that holds a resource, the way
# the Go resolver searches them (env.go resolveMultiHomeObserved).
beta_find_vault_resource() {
    local name="$1"
    local root="${WARP_VAULT_HOME:-${WARP_HOME:-/srv/warp}/vault}"
    local dir
    for dir in "$root" ${WARP_ENV:+"$root/$WARP_ENV"} "$root/all"; do
        if [[ -f "$dir/$name" ]]; then
            echo "$dir/$name"
            return 0
        fi
    done
    return 1
}

# Prints the scalar value of one top-level key, unquoted, with {{ env:X }}
# expanded. Only the matching line is read; nothing else in the file (the
# password) is ever printed.
beta_read_scalar() {
    local path="$1"
    local key="$2"
    local value
    local name
    value="$(sed -n -E "s/^${key}[[:space:]]*:[[:space:]]*//p" "$path" | head -n 1)"
    value="${value%%[[:space:]]#*}"
    value="${value%"${value##*[![:space:]]}"}"
    if [[ "$value" =~ ^\"(.*)\"$ || "$value" =~ ^\'(.*)\'$ ]]; then
        value="${BASH_REMATCH[1]}"
    fi
    while [[ "$value" =~ \{\{[[:space:]]*env:([A-Za-z_][A-Za-z0-9_]*)[[:space:]]*\}\} ]]; do
        name="${BASH_REMATCH[1]}"
        [[ -n "${!name:-}" ]] || beta_attest_error "$path names environment variable $name, which is not set"
        # split around the matched template; a ${value/a/b} replacement would
        # treat & in the value as the match
        value="${value%%"${BASH_REMATCH[0]}"*}${!name}${value#*"${BASH_REMATCH[0]}"}"
    done
    echo "$value"
}

# Appends the ip:port endpoints one authority resolves to.
beta_attest_authority() {
    local service="$1"
    local path="$2"
    local authority
    local host
    local port
    local resolved
    local address
    local count=0
    authority="$(beta_read_scalar "$path" authority)"
    if [[ "$authority" =~ ^\[(.+)\]:([0-9]+)$ || "$authority" =~ ^([^:]+):([0-9]+)$ ]]; then
        host="${BASH_REMATCH[1]}"
        port="${BASH_REMATCH[2]}"
    else
        beta_attest_error "$path has no host:port authority"
    fi
    (( 1 <= 10#$port && 10#$port <= 65535 )) || beta_attest_error "$service authority $authority has an invalid port"
    port="$((10#$port))"
    if [[ "$host" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ || "$host" == *:* ]]; then
        resolved="$host"
    elif ! resolved="$(getent ahosts "$host")"; then
        beta_attest_error "$service host $host (from $path) did not resolve; is the $service container up on this network?"
    fi
    for address in $(echo "$resolved" | awk '{ print $1 }' | LC_ALL=C sort -u); do
        address="${address#::ffff:}"
        if [[ ! "$address" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]]; then
            beta_attest_error "$service host $host resolves to $address; beta attests IPv4 endpoints only"
        fi
        if [[ "$address" =~ ^127\. || "$address" =~ ^0+(\.0+){3}$ || "$address" =~ ^(22[4-9]|23[0-9])\. ]]; then
            beta_attest_error "$service host $host resolves to $address; a loopback, unspecified or multicast endpoint is never attested"
        fi
        count=$((count + 1))
        case ",$BETA_ATTESTED_ADDRS," in
            *",$address:$port,"*) ;;
            *) BETA_ATTESTED_ADDRS="${BETA_ATTESTED_ADDRS:+$BETA_ATTESTED_ADDRS,}$address:$port" ;;
        esac
    done
    (( 0 < count )) || beta_attest_error "$service host $host resolved to no address"
    echo "[beta-entrypoint]local dial attestation: $service ${path##*/} $authority -> $(echo "$resolved" | awk '{ print $1 }' | LC_ALL=C sort -u | tr '\n' ' ')" >&2
}

BETA_WARP_ENV="${WARP_ENV:-}"
BETA_WARP_ENV="${BETA_WARP_ENV#"${BETA_WARP_ENV%%[![:space:]]*}"}"
BETA_WARP_ENV="${BETA_WARP_ENV%"${BETA_WARP_ENV##*[![:space:]]}"}"
if beta_local_environment "$BETA_WARP_ENV" && [[ -z "${WARP_TEST_ENV_ATTESTED_ADDRS:-}" ]]; then
    BETA_ATTESTED_ADDRS=""
    pg_path="$(beta_find_vault_resource pg.yml)" || beta_attest_error "vault pg.yml not found"
    beta_attest_authority postgres "$pg_path"
    if pg_maintenance_path="$(beta_find_vault_resource pg_maintenance.yml)"; then
        beta_attest_authority postgres "$pg_maintenance_path"
    fi
    redis_path="$(beta_find_vault_resource redis.yml)" || beta_attest_error "vault redis.yml not found"
    redis_cluster="$(beta_read_scalar "$redis_path" cluster)"
    if [[ "$redis_cluster" == true ]]; then
        beta_attest_error "$redis_path sets cluster: true; cluster node addresses cannot be attested at start"
    fi
    beta_attest_authority redis "$redis_path"
    IFS=, read -r -a beta_endpoints <<< "$BETA_ATTESTED_ADDRS"
    if (( 8 < ${#beta_endpoints[@]} )); then
        beta_attest_error "${#beta_endpoints[@]} endpoints to attest; the dial guard allows at most 8"
    fi
    export WARP_TEST_ENV_ATTESTED_ADDRS="$BETA_ATTESTED_ADDRS"
    echo "[beta-entrypoint]local dial attestation: WARP_TEST_ENV_ATTESTED_ADDRS=$WARP_TEST_ENV_ATTESTED_ADDRS" >&2
    unset BETA_ATTESTED_ADDRS beta_endpoints pg_path pg_maintenance_path redis_path redis_cluster
fi
unset BETA_WARP_ENV

exec "$@"
