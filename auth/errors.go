// SPDX-License-Identifier: BSD-3-Clause

package auth

import (
	"fmt"
	"strings"
)

// UnknownCapabilityError reports that a principal holds a capability the policy
// does not define. It is a configuration error (the supervision-tree spec or a
// role table referenced an undeclared capability), surfaced as a typed error so
// it fails closed at compile time rather than silently granting nothing.
type UnknownCapabilityError struct {
	Capability Capability
}

func (e *UnknownCapabilityError) Error() string {
	return fmt.Sprintf("auth: unknown capability %q", string(e.Capability))
}

// PolicyBuildError reports one or more problems found while compiling a policy.
type PolicyBuildError struct {
	Issues []string
}

func (e *PolicyBuildError) Error() string {
	return "auth: policy build failed: " + strings.Join(e.Issues, "; ")
}
