#!/usr/bin/env bash

set -euo pipefail

if [[ -z $BOT_TOKEN ]]; then echo "BOT_TOKEN var must be set"; exit 1; fi

if [[ -z $OPENSHIFT_PULL_SECRET ]]; then echo "OPENSHIFT_PULL_SECRET var must be set"; exit 1; fi

# Default to 3 max clusters for local testing
export MAX_CLUSTERS=${MAX_CLUSTERS:-3}

cd $(dirname $0)/..
make
./crc-cluster-bot \
  --v=2
