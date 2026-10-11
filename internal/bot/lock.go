package bot

import (
	"fmt"
	"net"
)

// AcquireInstanceLock stops a second copy of the bot from starting on the same
// machine. Two copies polling Telegram with the same token fight over updates
// and Telegram answers each with "Conflict: terminated by other getUpdates
// request", which the API library then retries forever.
//
// The lock is a listener on a loopback port: the operating system releases it
// automatically when the process exits, so there is no stale lock file to
// clean up, and it behaves the same on Linux, macOS, Windows and Android.
//
// It cannot see instances on other machines or inside other containers; those
// have to be found by hand.
func AcquireInstanceLock(port int) (net.Listener, error) {
	if port <= 0 {
		port = 47651
	}
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf(
			"another amdl-bot instance appears to be running on this machine (port %d is taken); "+
				"stop it first, or change telegram.instance-lock-port if the port is used by something else", port)
	}
	return l, nil
}
