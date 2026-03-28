#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
protoc --go_out=. --go_opt=module=github.com/palchrb/garmin-messenger-go \
  -Iproto proto/gm_android_checkin.proto proto/gm_checkin.proto
protoc --go_out=. --go_opt=module=github.com/palchrb/garmin-messenger-go \
  -Iproto proto/gm_mcs.proto
