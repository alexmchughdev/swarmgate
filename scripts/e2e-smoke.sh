#!/usr/bin/env bash
# Single-node end-to-end smoke test.
#
# Requires: local Docker with permission to manage it, git, a built
# dist/swarmgate (run `make build` first), and network access to Docker Hub
# for digest resolution.
#
# Flow: throwaway git repo with one stack -> swarmgate --once deploys it ->
# bump the image tag -> swarmgate --once updates it -> delete the stack file
# -> swarmgate --once prunes it. Asserts service state and telemetry after
# each cycle. Cleans up everything it created, including swarm mode if this
# script initialised it.
set -euo pipefail

SWARMGATE="$(cd "$(dirname "$0")/.." && pwd)/dist/swarmgate"
STACK=smoke
SERVICE=${STACK}_web
IMAGE_V1=nginx:1.27-alpine
IMAGE_V2=nginx:1.28-alpine

[ -x "$SWARMGATE" ] || { echo "FATAL: $SWARMGATE not built (run make build)"; exit 1; }
docker info >/dev/null 2>&1 || { echo "FATAL: docker daemon unreachable"; exit 1; }

WORK=$(mktemp -d)
INITIALISED_SWARM=0

cleanup() {
    docker service rm "$SERVICE" >/dev/null 2>&1 || true
    docker network rm "${STACK}_default" >/dev/null 2>&1 || true
    if [ "$INITIALISED_SWARM" = 1 ]; then
        docker swarm leave --force >/dev/null 2>&1 || true
    fi
    rm -rf "$WORK"
}
trap cleanup EXIT

if ! docker info 2>/dev/null | grep -q 'Swarm: active'; then
    docker swarm init >/dev/null
    INITIALISED_SWARM=1
    echo "initialised swarm mode (will leave on exit)"
fi

# --- throwaway desired-state repo -----------------------------------------
REPO="$WORK/repo"
git init -q -b main "$REPO"
git -C "$REPO" config user.name smoke
git -C "$REPO" config user.email smoke@localhost

write_stack() {
    cat > "$REPO/$STACK.yaml" <<EOF
services:
  web:
    image: $1
    deploy:
      replicas: 1
    healthcheck:
      test: ["CMD-SHELL", "wget -q --spider http://localhost/ || exit 1"]
      interval: 2s
      timeout: 2s
      retries: 3
EOF
    git -C "$REPO" add "$STACK.yaml"
    git -C "$REPO" commit -qm "$2"
}

write_stack "$IMAGE_V1" "initial stack"

EVENTS="$WORK/events.jsonl"
CONFIG="$WORK/swarmgate.yaml"
cat > "$CONFIG" <<EOF
git:
  url: "file://$REPO"
  branch: "main"
  path: "."
poll_interval: "5s"
prune: true
telemetry:
  out: "$EVENTS"
converge_timeout: "3m"
EOF

assert_event() { # stage [extra-grep]
    grep -q "\"stage\":\"$1\"" "$EVENTS" || { echo "FAIL: no $1 event"; exit 1; }
    if [ -n "${2:-}" ]; then
        grep "\"stage\":\"$1\"" "$EVENTS" | grep -q "$2" \
            || { echo "FAIL: $1 event missing $2"; exit 1; }
    fi
}

# --- cycle 1: initial deploy ----------------------------------------------
echo "cycle 1: deploy $IMAGE_V1"
"$SWARMGATE" --config "$CONFIG" --once

docker service inspect "$SERVICE" >/dev/null || { echo "FAIL: $SERVICE not created"; exit 1; }
assert_event converged
DIGEST_V1=$(docker service inspect -f '{{.Spec.TaskTemplate.ContainerSpec.Image}}' "$SERVICE")
case "$DIGEST_V1" in *@sha256:*) ;; *) echo "FAIL: image not digest-pinned: $DIGEST_V1"; exit 1;; esac
echo "deployed: $DIGEST_V1"

# --- cycle 2: image bump --------------------------------------------------
echo "cycle 2: update to $IMAGE_V2"
write_stack "$IMAGE_V2" "bump image"
"$SWARMGATE" --config "$CONFIG" --once

DIGEST_V2=$(docker service inspect -f '{{.Spec.TaskTemplate.ContainerSpec.Image}}' "$SERVICE")
[ "$DIGEST_V1" != "$DIGEST_V2" ] || { echo "FAIL: image unchanged after bump"; exit 1; }
[ "$(grep -c '"stage":"converged"' "$EVENTS")" -ge 2 ] || { echo "FAIL: second converged event missing"; exit 1; }
assert_event apply '"action":"update"'
echo "updated: $DIGEST_V2"

# --- cycle 3: prune --------------------------------------------------------
echo "cycle 3: remove stack (prune=true)"
git -C "$REPO" rm -q "$STACK.yaml"
git -C "$REPO" commit -qm "remove stack"
"$SWARMGATE" --config "$CONFIG" --once

if docker service inspect "$SERVICE" >/dev/null 2>&1; then
    echo "FAIL: $SERVICE still exists after prune"
    exit 1
fi
[ "$(grep -c '"stage":"converged"' "$EVENTS")" -ge 3 ] || { echo "FAIL: third converged event missing"; exit 1; }
assert_event apply '"action":"remove"'
echo "pruned: $SERVICE removed"

echo "--- telemetry ($EVENTS) ---"
cat "$EVENTS"
echo "SMOKE OK"
