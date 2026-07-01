package version

import (
	"fmt"
	"runtime"
)

var (
	Version   = "v0.0.1-dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

func Full() string {
	return fmt.Sprintf("%s (commit %s, built %s, %s/%s)",
		Version, Commit, BuildDate, runtime.GOOS, runtime.GOARCH)
}
