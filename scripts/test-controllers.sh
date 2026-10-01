#!/usr/bin/env bash

set -euo pipefail

API="http://localhost:8080"
COUNT=20

PASS="PASS"
FAIL="FAIL"

declare -A CREATED
declare -A UPDATED
declare -A DELETED

start=$(date +%s%N)

#
# All human-readable output goes to stderr.
#
log() {
    echo "$@" >&2
}

log_printf() {
    printf "$@" >&2
}


#
# Header
#
log
log "╔════════════════════════════════════════════╗"
log "║ Controlplane Integration Tests             ║"
log "╚════════════════════════════════════════════╝"
log

log "API server: ${API}"
log "Discovering registered resource kinds..."
log


#
# Check that the API server is reachable.
#
if ! curl \
    --connect-timeout 2 \
    --max-time 5 \
    -sf \
    "${API}/api/v1/kinds" >/dev/null; then

    log "ERROR: Cannot reach ${API}/api/v1/kinds"
    log "Make sure the API server is running."
    exit 1
fi


#
# Get registered resource kinds.
#
KINDS_JSON=$(
    curl \
        --connect-timeout 2 \
        --max-time 5 \
        -sf \
        "${API}/api/v1/kinds"
)


#
# Parse the kind list.
#
if ! command -v python3 >/dev/null 2>&1; then
    log "ERROR: python3 is required to parse /api/v1/kinds"
    exit 1
fi

mapfile -t KINDS < <(
    printf '%s\n' "$KINDS_JSON" |
        python3 -c '
import json
import sys

data = json.load(sys.stdin)

for item in data.get("items", []):
    api_version = item.get("apiVersion", "")
    kind = item.get("kind", "")
    resource = item.get("resource", "")

    if api_version and kind and resource:
        print(f"{api_version}|{kind}|{resource}")
'
)


#
# Make sure at least one kind is registered.
#
if [[ ${#KINDS[@]} -eq 0 ]]; then
    log "ERROR: The API server returned no registered resource kinds."
    log
    log "Response from /api/v1/kinds:"
    log "$KINDS_JSON"
    exit 1
fi


#
# Display discovered kinds.
#
log "Registered resource kinds:"
log

for entry in "${KINDS[@]}"; do
    IFS='|' read -r api_version kind resource <<< "$entry"

    log_printf "  %-20s %s\n" \
        "${api_version}/${kind}" \
        "(${resource})"
done

log


#
# Create resources.
#
create_resources() {
    local api_version=$1
    local kind=$2
    local spec

    CREATED[$kind]=0

    log "Creating ${COUNT} ${kind} resources..."

    for i in $(seq 1 "$COUNT"); do
        local name="controller-test-${i}"

        case "$kind" in
            DNSRecord)
                spec="{\"hostname\":\"${name}.example.test\",\"address\":\"192.0.2.10\"}"
                ;;
            Certificate)
                spec="{\"hostname\":\"${name}.example.test\",\"issuer\":\"internal-ca\"}"
                ;;
            *)
                spec="{\"name\":\"${name}\"}"
                ;;
        esac

        if curl \
            --connect-timeout 2 \
            --max-time 5 \
            -sf \
            -X POST \
            "${API}/api/v1/${kind}" \
            -H "Content-Type: application/json" \
            -d "{
                \"apiVersion\":\"${api_version}\",
                \"kind\":\"${kind}\",
                \"metadata\":{
                    \"name\":\"${name}\"
                },
                "spec":${spec}
            }" >/dev/null; then

            CREATED[$kind]=$((CREATED[$kind] + 1))
        fi
    done
}


#
# Update resources.
#
update_resources() {
    local api_version=$1
    local kind=$2
    local spec

    UPDATED[$kind]=0

    log "Updating ${COUNT} ${kind} resources..."

    for i in $(seq 1 "$COUNT"); do
        local name="controller-test-${i}"

        case "$kind" in
            DNSRecord)
                spec="{\"hostname\":\"updated-${name}.example.test\",\"address\":\"192.0.2.11\"}"
                ;;
            Certificate)
                spec="{\"hostname\":\"updated-${name}.example.test\",\"issuer\":\"internal-ca\"}"
                ;;
            *)
                spec="{\"name\":\"${name}\",\"updated\":true}"
                ;;
        esac

        if curl \
            --connect-timeout 2 \
            --max-time 5 \
            -sf \
            -X PUT \
            "${API}/api/v1/${kind}/${name}" \
            -H "Content-Type: application/json" \
            -d "{
                \"apiVersion\":\"${api_version}\",
                \"kind\":\"${kind}\",
                \"metadata\":{
                    \"name\":\"${name}\"
                },
                "spec":${spec}
            }" >/dev/null; then

            UPDATED[$kind]=$((UPDATED[$kind] + 1))
        fi
    done
}


#
# Delete resources.
#
delete_resources() {
    local kind=$1

    DELETED[$kind]=0

    log "Deleting ${COUNT} ${kind} resources..."

    for i in $(seq 1 "$COUNT"); do
        local name="controller-test-${i}"

        if curl \
            --connect-timeout 2 \
            --max-time 5 \
            -sf \
            -X DELETE \
            "${API}/api/v1/${kind}/${name}" >/dev/null; then

            DELETED[$kind]=$((DELETED[$kind] + 1))
        fi
    done
}


#
# Test every registered resource kind.
#
for entry in "${KINDS[@]}"; do
    IFS='|' read -r api_version kind resource <<< "$entry"

    log
    log "Testing ${api_version}/${kind}"
    log "--------------------------------------------"

    create_resources "$api_version" "$kind"
    update_resources "$api_version" "$kind"
    delete_resources "$kind"
done


end=$(date +%s%N)

duration=$(( (end - start) / 1000000 ))


#
# Print results.
#
log
log_printf "%-20s %8s %8s %8s %10s\n" \
    "Resource" \
    "Created" \
    "Updated" \
    "Deleted" \
    "Result"

log_printf "%-20s %8s %8s %8s %10s\n" \
    "--------------------" \
    "--------" \
    "--------" \
    "--------" \
    "----------"


FAILED=0
TOTAL_OPERATIONS=0

for entry in "${KINDS[@]}"; do
    IFS='|' read -r api_version kind resource <<< "$entry"

    result=$PASS

    if [[ ${CREATED[$kind]:-0} != "$COUNT" ||
          ${UPDATED[$kind]:-0} != "$COUNT" ||
          ${DELETED[$kind]:-0} != "$COUNT" ]]; then

        result=$FAIL
        FAILED=1
    fi

    TOTAL_OPERATIONS=$((TOTAL_OPERATIONS + COUNT * 3))

    log_printf "%-20s %8s %8s %8s %10s\n" \
        "$kind" \
        "${CREATED[$kind]:-0}" \
        "${UPDATED[$kind]:-0}" \
        "${DELETED[$kind]:-0}" \
        "$result"
done


#
# Summary.
#
log
log "Operations: ${TOTAL_OPERATIONS}"
log "Duration: ${duration}ms"
log

if [[ $FAILED -eq 1 ]]; then
    log "Tests failed"
    exit 1
fi

log "All controller tests passed"
