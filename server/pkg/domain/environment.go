package domain

import "regexp"

// deployEnvRE bounds a deploy environment name (#39): starts alphanumeric, then
// alphanumerics / dot / dash / underscore, max 64 chars. Shared by the pipeline
// parser (validating `deploy.environment`) and the deploy-target registry, so the
// API can't register a target for an environment no pipeline could reference — and
// a name with '/' can't break the target's DELETE route.
var deployEnvRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// ValidEnvironmentName reports whether s is a valid deploy environment name.
func ValidEnvironmentName(s string) bool {
	return deployEnvRE.MatchString(s)
}
