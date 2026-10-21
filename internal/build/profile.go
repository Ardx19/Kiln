package build

import (
	"strconv"
	"strings"

	"github.com/Ardx19/kiln/internal/depfile"
)

// Profile is the Ninja release kiln behaves like in a given build tree,
// identified by the build log version that release writes. Sharing a tree
// with Ninja means matching it in everything that leaves a trace: the log
// format and hash, what mtime gets recorded, how depfiles parse and which
// exit codes come back.
type Profile struct {
	LogVersion int // 5 (Ninja 1.10–1.11), 6 (1.12) or 7 (1.13)
}

// ProfileForNinja returns the profile of a Ninja version string such as
// "1.11.1". Unparseable versions get the newest profile.
func ProfileForNinja(version string) Profile {
	parts := strings.SplitN(strings.TrimSpace(version), ".", 3)
	if len(parts) >= 2 {
		major, err1 := strconv.Atoi(parts[0])
		minor, err2 := strconv.Atoi(parts[1])
		if err1 == nil && err2 == nil && major == 1 {
			switch {
			case minor < 12:
				return Profile{LogVersion: 5}
			case minor < 13:
				return Profile{LogVersion: 6}
			}
		}
	}
	return Profile{LogVersion: 7}
}

// RecordsStartTime: from Ninja 1.12, the log records when each command
// started (the mtime of a lock file touched at that moment), not when its
// outputs were written.
func (p Profile) RecordsStartTime() bool { return p.LogVersion >= 6 }

// PropagatesExitCodes: Ninja 1.13 exits with the failing command's own
// status and with 130 when interrupted; earlier releases exit 1 and 2.
func (p Profile) PropagatesExitCodes() bool { return p.LogVersion >= 7 }

// DepfileDialect is how this Ninja parses depfiles.
func (p Profile) DepfileDialect() depfile.Dialect { return depfile.DialectForLog(p.LogVersion) }

// FailureExit is the exit code for a build that failed because a command
// exited with status (or was killed by a signal when status < 0).
func (p Profile) FailureExit(status int) int {
	if !p.PropagatesExitCodes() {
		return 1
	}
	return status
}

// InterruptExit is the exit code for an interrupted build.
func (p Profile) InterruptExit() int {
	if p.PropagatesExitCodes() {
		return 130
	}
	return 2
}
