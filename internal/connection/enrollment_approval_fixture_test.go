package connection

import "context"

// Fixture approval is explicit at each StartEnrollment call. Production callers
// must provide their own operator approval; there is no default allow callback.
func approveEnrollmentForTest(context.Context, EnrollmentCandidate) error { return nil }
