#!/usr/bin/env bash
# Shared throwaway-Postgres helper for the Docker-bound tiers. `start_postgres <label>`
# boots the pinned image on a random loopback port and echoes the connection URL;
# `stop_postgres` removes the container + volume and fails if either leaked. The
# credential is minted per run and never written to a file (same contract as the
# component tier, which keeps its own inline copy).

# shellcheck shell=bash

_pg_container=""
_pg_volume=""
_pg_label=""

start_postgres() {
  # Immutable pinned image (matches the postgres-coordinator spike / ADR-0001).
  local image="postgres@sha256:17e67d7b9890c99b055ba1e0d5c5be4ec27c9d3a72bda32db24a5e5d8a85af0c"
  local run_id="$$-${RANDOM}"
  _pg_label="$1"
  _pg_container="palai-${_pg_label##*=}-postgres-$run_id"
  _pg_volume="$_pg_container"
  local password="palai-pg-${RANDOM}${RANDOM}"

  docker volume create --label "$_pg_label" "$_pg_volume" >/dev/null
  docker run --detach --pull=never \
    --name "$_pg_container" \
    --label "$_pg_label" \
    --env POSTGRES_DB=palai \
    --env POSTGRES_PASSWORD="$password" \
    --publish 127.0.0.1::5432 \
    --volume "$_pg_volume:/var/lib/postgresql/data" \
    "$image" >/dev/null

  local ready=0
  for _ in $(seq 1 120); do
    if docker exec "$_pg_container" pg_isready --username postgres --dbname palai >/dev/null 2>&1; then
      ready=1
      break
    fi
    sleep 0.25
  done
  if test "$ready" -ne 1; then
    docker logs "$_pg_container" >&2 || true
    echo "postgres container did not become ready" >&2
    return 1
  fi

  local binding port
  binding="$(docker port "$_pg_container" 5432/tcp)"
  port="${binding##*:}"
  echo "postgres://postgres:$password@127.0.0.1:$port/palai?sslmode=disable"
}

postgres_logs() { test -n "$_pg_container" && docker logs "$_pg_container" >&2 || true; }

stop_postgres() {
  test -n "$_pg_container" || return 0
  docker rm -f "$_pg_container" >/dev/null 2>&1 || true
  docker volume rm -f "$_pg_volume" >/dev/null 2>&1 || true
  local leaked
  leaked="$(docker ps -aq --filter "label=$_pg_label" | sed '/^$/d' | wc -l | tr -d ' ')"
  test "$leaked" -eq 0 || { echo "postgres suite leaked a container" >&2; return 1; }
  _pg_container=""
}
