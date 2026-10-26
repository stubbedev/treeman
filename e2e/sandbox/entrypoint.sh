#!/usr/bin/env bash
set -euo pipefail

dind dockerd --host=unix:///var/run/docker.sock >/var/log/dockerd.log 2>&1 &

deadline=$((SECONDS + 60))
until docker info >/dev/null 2>&1; do
    if [ "$SECONDS" -ge "$deadline" ]; then
        echo "sandbox: dockerd did not become ready within 60s; log:" >&2
        cat /var/log/dockerd.log >&2
        exit 1
    fi
    sleep 0.5
done

mapfile -t leftover < <(docker ps -aq)
if [ "${#leftover[@]}" -gt 0 ]; then
    docker rm -f "${leftover[@]}" >/dev/null
fi
docker volume prune -af >/dev/null
docker network prune -f >/dev/null

if [ -n "${DOCKERHUB_USERNAME:-}" ] && [ -n "${DOCKERHUB_TOKEN:-}" ]; then
    printf '%s' "$DOCKERHUB_TOKEN" | docker login -u "$DOCKERHUB_USERNAME" --password-stdin >/dev/null
fi

exec "$@"
