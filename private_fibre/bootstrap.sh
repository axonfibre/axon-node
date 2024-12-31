#!/bin/bash

if [[ "$OSTYPE" != "darwin"* && "$EUID" -ne 0 ]]; then
  echo "Please run as root or with sudo"
  exit
fi

# Cleanup if necessary
if [ -d "privatedb" ] || [ -d "snapshots" ]; then
  ./cleanup.sh
fi

if [[ $1 = "build" ]]; then
  # Build latest code
  docker compose --profile "bootstrap" build

  # Pull latest images
  docker compose pull inx-coordinator
  docker compose pull inx-indexer
  docker compose pull inx-mqtt
  docker compose pull inx-faucet
  docker compose pull inx-participation
  docker compose pull inx-spammer
  docker compose pull inx-poi
  docker compose pull inx-dashboard-1
fi

# Create snapshot
mkdir -p snapshots/hornet-1
if [[ "$OSTYPE" != "darwin"* ]]; then
  chown -R 65532:65532 snapshots
fi
docker compose run create-snapshots

# Prepare database directory for axon-node-1
mkdir -p privatedb/axon-node-1
mkdir -p privatedb/state
if [[ "$OSTYPE" != "darwin"* ]]; then
  chown -R 65532:65532 privatedb
fi

# Bootstrap network (create hornet-1 database, create genesis milestone, create coo state)
docker compose run bootstrap-network

# Duplicate snapshot for all nodes
cp -R snapshots/axon-node-1 snapshots/axon-node-2
cp -R snapshots/axon-node-1 snapshots/axon-node-3
cp -R snapshots/axon-node-1 snapshots/axon-node-4
if [[ "$OSTYPE" != "darwin"* ]]; then
  chown -R 65532:65532 snapshots
fi

# Prepare database directory
mkdir -p privatedb/indexer
mkdir -p privatedb/participation
mkdir -p privatedb/axon-node-2
mkdir -p privatedb/axon-node-3
mkdir -p privatedb/axon-node-4
if [[ "$OSTYPE" != "darwin"* ]]; then
  chown -R 65532:65532 privatedb
fi
