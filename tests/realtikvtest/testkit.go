// Copyright 2022 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !codes

package realtikvtest

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pingcap/tidb/pkg/config"
	"github.com/pingcap/tidb/pkg/config/kerneltype"
	"github.com/pingcap/tidb/pkg/ddl"
	"github.com/pingcap/tidb/pkg/ddl/ingest/testutil"
	"github.com/pingcap/tidb/pkg/domain"
	"github.com/pingcap/tidb/pkg/keyspace"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/session"
	"github.com/pingcap/tidb/pkg/sessionctx/vardef"
	kvstore "github.com/pingcap/tidb/pkg/store"
	"github.com/pingcap/tidb/pkg/store/driver"
	"github.com/pingcap/tidb/pkg/testkit"
	"github.com/pingcap/tidb/pkg/testkit/testmain"
	"github.com/pingcap/tidb/pkg/testkit/testsetup"
	"github.com/stretchr/testify/require"
	"github.com/tikv/client-go/v2/tikv"
	"github.com/tikv/client-go/v2/txnkv/transaction"
	"go.opencensus.io/stats/view"
	"go.uber.org/goleak"
)

var (
	// WithRealTiKV is a flag identify whether tests run with real TiKV
	WithRealTiKV = flag.Bool("with-real-tikv", false, "whether tests run with real TiKV")

	// TiKVPath is the path of the TiKV Storage.
	TiKVPath = flag.String("tikv-path", "tikv://127.0.0.1:2379?disableGC=true", "TiKV addr")

	// PDAddr is the address of PD.
	PDAddr = "127.0.0.1:2379"
)

// RunTestMain run common setups for all real tikv tests.
func RunTestMain(m *testing.M) {
	testsetup.SetupForCommonTest()
	*WithRealTiKV = true
	flag.Parse()
	session.SetSchemaLease(5 * time.Second)
	config.UpdateGlobal(func(conf *config.Config) {
		conf.TiKVClient.AsyncCommit.SafeWindow = 0
		conf.TiKVClient.AsyncCommit.AllowedClockDrift = 0
	})
	tikv.EnableFailpoints()
	opts := []goleak.Option{
		goleak.IgnoreTopFunction("github.com/golang/glog.(*fileSink).flushDaemon"),
		goleak.IgnoreTopFunction("github.com/bazelbuild/rules_go/go/tools/bzltestutil.RegisterTimeoutHandler.func1"),
		goleak.IgnoreTopFunction("github.com/lestrrat-go/httprc.runFetchWorker"),
		goleak.IgnoreTopFunction("github.com/tikv/client-go/v2/config/retry.newBackoffFn.func1"),
		goleak.IgnoreTopFunction("go.etcd.io/etcd/client/v3.waitRetryBackoff"),
		goleak.IgnoreTopFunction("go.etcd.io/etcd/client/pkg/v3/logutil.(*MergeLogger).outputLoop"),
		goleak.IgnoreTopFunction("google.golang.org/grpc.(*addrConn).resetTransport"),
		goleak.IgnoreTopFunction("google.golang.org/grpc.(*ccBalancerWrapper).watcher"),
		goleak.IgnoreTopFunction("google.golang.org/grpc/internal/transport.(*controlBuffer).get"),
		// top function of this routine might be "sync.runtime_notifyListWait(0xc0098f5450, 0x0)", so we use IgnoreAnyFunction.
		goleak.IgnoreAnyFunction("google.golang.org/grpc/internal/transport.(*http2Client).keepalive"),
		goleak.IgnoreTopFunction("google.golang.org/grpc/internal/grpcsync.(*CallbackSerializer).run"),
		goleak.IgnoreTopFunction("internal/poll.runtime_pollWait"),
		goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
		goleak.IgnoreTopFunction("github.com/tikv/client-go/v2/txnkv/transaction.keepAlive"),
		// backoff function will lead to sleep, so there is a high probability of goroutine leak while it's doing backoff.
		goleak.IgnoreTopFunction("github.com/tikv/client-go/v2/config/retry.(*Config).createBackoffFn.newBackoffFn.func2"),
		goleak.IgnoreTopFunction("go.opencensus.io/stats/view.(*worker).start"),
		// the resolveFlushedLocks goroutine runs in the background to commit or rollback locks.
		goleak.IgnoreAnyFunction("github.com/tikv/client-go/v2/txnkv/transaction.(*twoPhaseCommitter).resolveFlushedLocks.func1"),
		goleak.Cleanup(testutil.CheckIngestLeakageForTest),
	}
	callback := func(i int) int {
		// wait for MVCCLevelDB to close, MVCCLevelDB will be closed in one second
		time.Sleep(time.Second)
		return i
	}
	goleak.VerifyTestMain(testmain.WrapTestingM(m, callback), opts...)
}

type realtikvStoreOption struct {
	retainData bool
	keyspace   string
}

// RealTiKVStoreOption is the config option for creating a real TiKV store.
type RealTiKVStoreOption func(opt *realtikvStoreOption)

// WithRetainData allows the store to retain old data when creating a new store.
func WithRetainData() RealTiKVStoreOption {
	return func(opt *realtikvStoreOption) {
		opt.retainData = true
	}
}

// WithKeyspaceName allows the store to use a specific keyspace name.
func WithKeyspaceName(name string) RealTiKVStoreOption {
	return func(opt *realtikvStoreOption) {
		opt.keyspace = name
	}
}

// CreateMockStoreAndSetup return a new kv.Storage.
func CreateMockStoreAndSetup(t *testing.T, opts ...RealTiKVStoreOption) kv.Storage {
	store, _ := CreateMockStoreAndDomainAndSetup(t, opts...)
	return store
}

// CreateMockStoreAndDomainAndSetup initializes a kv.Storage and a domain.Domain.
func CreateMockStoreAndDomainAndSetup(t *testing.T, opts ...RealTiKVStoreOption) (kv.Storage, *domain.Domain) {
	//nolint: errcheck
	_ = kvstore.Register(config.StoreTypeTiKV, &driver.TiKVDriver{})
	kvstore.SetSystemStorage(nil)
	// set it to 5 seconds for testing lock resolve.
	atomic.StoreUint64(&transaction.ManagedLockTTL, 5000)
	transaction.PrewriteMaxBackoff.Store(500)

	var store kv.Storage
	var dom *domain.Domain
	var err error

	option := &realtikvStoreOption{}
	for _, opt := range opts {
		opt(option)
	}
	var ks string
	if kerneltype.IsNextGen() {
		if option.keyspace == "" {
			// in nextgen kernel, SYSTEM keyspace must be bootstrapped first, if we
			// don't specify a keyspace which normally is not specified, we use SYSTEM
			// keyspace as default to make sure test cases can run correctly.
			ks = keyspace.System
		} else {
			ks = option.keyspace
		}
		t.Log("create realtikv store with keyspace:", ks)
	}
	session.SetSchemaLease(500 * time.Millisecond)

	path := *TiKVPath
	if len(ks) > 0 {
		path += "&keyspaceName=" + ks
	}
	var d driver.TiKVDriver
	config.UpdateGlobal(func(conf *config.Config) {
		conf.TxnLocalLatches.Enabled = false
		conf.KeyspaceName = ks
		conf.Store = config.StoreTypeTiKV
	})
	store, err = d.Open(path)
	require.NoError(t, err)
	if kerneltype.IsNextGen() && ks != keyspace.System {
		sysPath := *TiKVPath + "&keyspaceName=" + keyspace.System
		sysStore, err := d.Open(sysPath)
		require.NoError(t, err)
		kvstore.SetSystemStorage(sysStore)
		t.Cleanup(func() {
			require.NoError(t, sysStore.Close())
		})
	}
	require.NoError(t, ddl.StartOwnerManager(context.Background(), store))
	dom, err = session.BootstrapSession(store)
	require.NoError(t, err)
	sm := testkit.MockSessionManager{}
	dom.InfoSyncer().SetSessionManager(&sm)
	tk := testkit.NewTestKit(t, store)
	// set it to default value.
	tk.MustExec(fmt.Sprintf("set global innodb_lock_wait_timeout = %d", vardef.DefInnodbLockWaitTimeout))
	tk.MustExec("use test")

	if !option.retainData {
		tk.MustExec("delete from mysql.tidb_global_task;")
		tk.MustExec("delete from mysql.tidb_background_subtask;")
		tk.MustExec("delete from mysql.tidb_ddl_job;")
		rs := tk.MustQuery("show tables")
		tables := []string{}
		for _, row := range rs.Rows() {
			tables = append(tables, fmt.Sprintf("`%v`", row[0]))
		}
		for _, table := range tables {
			tk.MustExec(fmt.Sprintf("alter table %s nocache", table))
		}
		if len(tables) > 0 {
			tk.MustExec(fmt.Sprintf("drop table %s", strings.Join(tables, ",")))
		}
		t.Log("cleaned up ddl and tables")
	}

	t.Cleanup(func() {
		dom.Close()
		ddl.CloseOwnerManager()
		require.NoError(t, store.Close())
		transaction.PrewriteMaxBackoff.Store(20000)
		view.Stop()
	})
	return store, dom
}
