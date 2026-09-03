#!/bin/sh
# Built from the repository root: the Dockerfile needs go.mod and internal/ too.
set -e
cd "$(dirname "$0")/.."
docker build -f agent/Dockerfile -t backio-agent:latest .
