package version

import "runtime"

const Name = "gosmo"

var (
	Version = "v0.0.2"
	Commit  = "unknown"
	Date    = "unknown"
)

func Runtime() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}
