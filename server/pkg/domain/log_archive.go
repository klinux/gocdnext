package domain

// LogArchivePolicy is what the GOCDNEXT_LOG_ARCHIVE env parses
// to. The defined values mirror what the operator types verbatim;
// anything else falls through to LogArchiveAuto in the resolver
// (forgiving on stray whitespace, lowercase mismatch, etc.).
type LogArchivePolicy string

const (
	// LogArchiveAuto enables archiving iff an artifact backend is
	// wired. Sensible default for most deployments — the platform
	// makes the call based on what's available.
	LogArchiveAuto LogArchivePolicy = "auto"
	// LogArchiveOn forces archiving on. Boot-time validation rejects
	// this when no artifact backend is configured (otherwise every
	// job would queue an archive that can't ship anywhere).
	LogArchiveOn LogArchivePolicy = "on"
	// LogArchiveOff turns archiving off cluster-wide regardless of
	// per-project flags — the kill switch.
	LogArchiveOff LogArchivePolicy = "off"
)

// EffectiveLogArchive folds the three inputs (global policy,
// per-project override, artifact-store availability) into the
// concrete "should this job's logs be archived?" decision.
//
//   - projectFlag NIL  -> inherit global.
//   - projectFlag set  -> override wins (project opted in or out
//     explicitly), but ONLY when the global policy isn't Off.
//     "off" is a hard cluster-wide disable; respecting a project's
//     "always on" past that would defeat the kill switch.
//   - global=auto     -> true iff hasStore.
//   - global=on       -> true iff hasStore (caller is responsible
//     for boot-time validation; we still gate so a misconfigured
//     deploy doesn't enqueue impossible work).
//   - global=off      -> false, project flag ignored.
//
// Anything that isn't recognised maps to LogArchiveAuto. Defensive:
// a typo'd env shouldn't break archive scheduling.
func EffectiveLogArchive(global LogArchivePolicy, projectFlag *bool, hasStore bool) bool {
	switch global {
	case LogArchiveOff:
		return false
	case LogArchiveOn:
		if projectFlag != nil {
			return *projectFlag && hasStore
		}
		return hasStore
	default: // LogArchiveAuto + anything unknown
		if projectFlag != nil {
			return *projectFlag && hasStore
		}
		return hasStore
	}
}
