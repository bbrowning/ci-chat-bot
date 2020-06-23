#!/usr/bin/env bash

set -euo pipefail

if [[ -z $BOT_TOKEN ]]; then echo "BOT_TOKEN var must be set"; exit 1; fi

tmp_kk=$(mktemp)
trap 'rm -rf $tmp_kk' EXIT

kubectl config view >$tmp_kk
kubectl --kubeconfig=$tmp_kk config use-context api.ci


kubectl config use-context app.ci
cd $(dirname $0)/..
make
./crc-cluster-bot \
  --build-cluster-kubeconfig=$tmp_kk \
  --v=2
