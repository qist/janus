package work

import (
	"fmt"
	"strings"
)

// SelfReview reports whether the same agent that submitted the item is now
// trying to verify it. Changing role or session does not make a review
// independent, so the check is on agent identity.
func (item WorkItem) SelfReview(agentID string) bool {
	submitted := strings.TrimSpace(item.SubmittedBy)
	return submitted != "" && submitted == agentID
}

// validateEvidence enforces the contract at submit time:
//
//   - at least one artifact or a diff hash, so the verifier has something to
//     inspect;
//   - a real result for every check declared in the contract, exiting with the
//     expected code.
//
// Results carry the command's actual exit code, so passing a hand-written
// "Passed: true" is not enough.
func validateEvidence(c Contract, ev Evidence) error {
	if len(ev.Artifacts) == 0 && strings.TrimSpace(ev.DiffHash) == "" {
		return ErrEvidenceMissing
	}

	results := make(map[string]CheckResult, len(ev.Checks))
	for _, r := range ev.Checks {
		results[strings.TrimSpace(r.Name)] = r
	}

	for _, check := range c.Checks {
		name := strings.TrimSpace(check.Name)
		if name == "" {
			continue
		}
		result, ok := results[name]
		if !ok {
			return fmt.Errorf("%w: %s", ErrCheckNotRun, name)
		}
		if !result.Passed(check.ExpectExit) {
			return fmt.Errorf("%w: %s exit=%d want %d", ErrCheckFailed, name, result.ExitCode, check.ExpectExit)
		}
	}
	return nil
}
