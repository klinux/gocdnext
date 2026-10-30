package domain

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// ApproverSubjectPrefix marks an `approvers:` entry that names a person by
// their stable IdP subject instead of a display string:
//
//	sub:<provider>:<subject>
//
// <provider> is the gocdnext auth provider name (users.provider) and <subject>
// the IdP's stable id for the person (users.external_id — the OIDC `sub`, the
// GitHub numeric id). Unlike a name or email it survives renames and email
// changes, and — because the IdP signs it — another source of display names
// (an embedding platform, a second IdP) can't drift it (#172).
//
// An entry carrying this prefix matches ONLY by subject, never by name/email:
// otherwise a user able to set their own display name to "sub:oidc:<victim>"
// would satisfy the victim's entry through the label path.
const ApproverSubjectPrefix = "sub:"

// Bounds on a subject ref. OIDC Core caps `sub` at 255 ASCII chars; provider
// names are short operator-chosen labels.
const (
	maxApproverProviderLen = 64
	maxApproverSubjectLen  = 255
)

// ApproverSubjectRef renders the subject ref for a provider + subject, or ""
// when either is missing (an identity with no stable subject can't be named
// this way, and "" never matches an allow-list entry).
func ApproverSubjectRef(provider, subject string) string {
	if provider == "" || subject == "" {
		return ""
	}
	return ApproverSubjectPrefix + provider + ":" + subject
}

// ValidateApproverRef checks one `approvers:` entry. Plain entries (name or
// email) pass as-is — their shape predates subject refs and stays untouched.
// Entries with the subject prefix must be `sub:<provider>:<subject>` with both
// parts present, bounded, and free of whitespace/control characters; the
// subject may itself contain ':' (URN-style subs), the provider may not.
//
// Errors describe the defect without echoing the entry: a subject ref is an
// IdP identifier and doesn't belong in parse errors or logs.
func ValidateApproverRef(entry string) error {
	rest, ok := strings.CutPrefix(entry, ApproverSubjectPrefix)
	if !ok {
		return nil
	}
	provider, subject, ok := strings.Cut(rest, ":")
	switch {
	case !ok:
		return errors.New("subject approver must be sub:<provider>:<subject>")
	case provider == "":
		return errors.New("subject approver is missing the provider")
	case subject == "":
		return errors.New("subject approver is missing the subject")
	case len(provider) > maxApproverProviderLen:
		return fmt.Errorf("subject approver provider exceeds %d bytes", maxApproverProviderLen)
	case len(subject) > maxApproverSubjectLen:
		return fmt.Errorf("subject approver subject exceeds %d bytes", maxApproverSubjectLen)
	case strings.IndexFunc(rest, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0:
		return errors.New("subject approver contains whitespace or control characters")
	}
	return nil
}
