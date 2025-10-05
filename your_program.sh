#!/bin/sh

# Start server by default; run CLI with --cli
if [ "$1" = "--cli" ]; then
  shift
  exec go run ./cmd/mini-redis-cli "$@"
else
  exec go run ./cmd/mini-redis-server "$@"
fi

 