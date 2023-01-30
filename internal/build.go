package internal

import "runtime/debug"

var (
	AppName    = "bbb-webrtc-recorder"
	AppVersion = "devel"
	ModName    string

	BuildInfo *debug.BuildInfo
)

func init() {
	BuildInfo, _ = debug.ReadBuildInfo()
	ModName = BuildInfo.Main.Path
}
