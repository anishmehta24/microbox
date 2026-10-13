// Package spec holds what the runtime (parent) and init (child) share: the
// container config and the exit codes.
package spec

// Config is everything init needs to set up the container. The runtime sends it
// as JSON over the init pipe.
type Config struct {
	ID       string   `json:"id"`
	Hostname string   `json:"hostname"`
	Rootfs   string   `json:"rootfs"` // absolute path on the host
	Args     []string `json:"args"`
	Env      []string `json:"env"`
}

// InitPipeFD is the file descriptor init reads its Config from. It is the first
// entry of exec.Cmd.ExtraFiles, which always lands on fd 3.
const InitPipeFD = 3

// Exit codes for failures before the user's command runs. Same meaning as in
// docker run: 125 is a runtime error, 126 means the command could not be
// executed, 127 means it was not found.
const (
	ExitRuntimeError = 125
	ExitCannotInvoke = 126
	ExitNotFound     = 127
)
