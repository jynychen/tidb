#!/bin/bash
#
# Copyright 2021 PingCAP, Inc.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -eux

CUR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

LOG_FILE1="$TEST_DIR/lightning-distributed-import1.log"
LOG_FILE2="$TEST_DIR/lightning-distributed-import2.log"

# let lightning run a bit slow to avoid some table in the first lightning finish too fast.
export GO_FAILPOINTS="github.com/pingcap/tidb/lightning/pkg/importer/SlowDownImport=sleep(2500)"

run_lightning --backend local --sorted-kv-dir "$TEST_DIR/lightning_distributed_import.sorted1" \
  -d "$CUR/data1" --log-file "$LOG_FILE1" --config "$CUR/config.toml" &
pid1="$!"

# sleep 1 second to avoid both lightning starting at the same time and have same ID.
sleep 1

run_lightning --backend local --sorted-kv-dir "$TEST_DIR/lightning_distributed_import.sorted2" \
  -d "$CUR/data2" --log-file "$LOG_FILE2" --config "$CUR/config.toml" &
pid2="$!"

wait "$pid1" "$pid2"

run_sql 'select count(*) from distributed_import.t'
check_contains 'count(*): 10'
