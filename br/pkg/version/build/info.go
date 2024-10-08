// Copyright 2021 PingCAP, Inc. Licensed under Apache-2.0.

package build

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"

	"github.com/pingcap/log"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/util/israce"
	"github.com/pingcap/tidb/pkg/util/versioninfo"
	"go.uber.org/zap"
)

// Version information.
var (
	ReleaseVersion        = getReleaseVersion()
	BuildTS               = versioninfo.TiDBBuildTS
	GitHash               = versioninfo.TiDBGitHash
	GitBranch             = versioninfo.TiDBGitBranch
	goVersion             = runtime.Version()
	ReleaseVersionForTest = "nightly-dirty"
)

func getReleaseVersion() string {
	if mysql.TiDBReleaseVersion != "None" && !strings.Contains(mysql.TiDBReleaseVersion, "this-is-a-placeholder") {
		return mysql.TiDBReleaseVersion
	}
	// it's unreachable for normal path, only for realtikv tests
	// we need to set the ReleaseVersion manually.
	return ReleaseVersionForTest
}

// AppName is a name of a built binary.
type AppName string

var (
	// BR is the name of BR binary.
	BR AppName = "Backup & Restore (BR)"
	// Lightning is the name of Lightning binary.
	Lightning AppName = "TiDB-Lightning"
)

// LogInfo logs version information.
func LogInfo(name AppName) {
	oldLevel := log.GetLevel()
	log.SetLevel(zap.InfoLevel)
	defer log.SetLevel(oldLevel)

	log.Info(fmt.Sprintf("Welcome to %s", name),
		zap.String("release-version", ReleaseVersion),
		zap.String("git-hash", GitHash),
		zap.String("git-branch", GitBranch),
		zap.String("go-version", goVersion),
		zap.String("utc-build-time", BuildTS),
		zap.Bool("race-enabled", israce.RaceEnabled))
}

// Info returns version information.
func Info() string {
	buf := bytes.Buffer{}
	fmt.Fprintf(&buf, "Release Version: %s\n", ReleaseVersion)
	fmt.Fprintf(&buf, "Git Commit Hash: %s\n", GitHash)
	fmt.Fprintf(&buf, "Git Branch: %s\n", GitBranch)
	fmt.Fprintf(&buf, "Go Version: %s\n", goVersion)
	fmt.Fprintf(&buf, "UTC Build Time: %s\n", BuildTS)
	fmt.Fprintf(&buf, "Race Enabled: %t", israce.RaceEnabled)
	return buf.String()
}
