#!/bin/bash

retry() {
  for i in {1..3}; do
    "${@}" && return 0
  done
  return 1
}

# Fail on any error.
set -eo pipefail

# Display commands being run.
set -x

# Record directory of pprof-nodejs
cd $(dirname $0)/..
BASE_DIR=$(pwd)

RUN_ONLY_V8_CANARY_TEST="${RUN_ONLY_V8_CANARY_TEST:-false}"
echo "$RUN_ONLY_V8_CANARY_TEST"

# Move test to go path.
export GOPATH="$HOME/go"
mkdir -p "$GOPATH/src"

cp -R "system-test" "$GOPATH/src/pproftest"

# Run test.
cd "$GOPATH/src/pproftest"
retry go get -t -d .
go test -v -timeout=10m -tags=integration -run TestAgentIntegration  -binary_host="$BINARY_HOST" -pprof_nodejs_path="$BASE_DIR" -run_only_v8_canary_test="$RUN_ONLY_V8_CANARY_TEST"
