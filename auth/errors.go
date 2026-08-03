// SPDX-License-Identifier: BSD-3-Clause

package auth

import (
	"errors"
	"fmt"
	"strings"
)

// Registry errors.
var (
	// ErrEmptyKey is returned when registering an empty public key.
	ErrEmptyKey = errors.New("auth: empty public key")
	// ErrKeyRegistered is returned when registering a key that is already
	// registered; rotate by revoking the old key first.
	ErrKeyRegistered = errors.New("auth: public key already registered")
	// ErrKeyRevoked is returned when registering a key that was revoked; a
	// revoked per-boot key value must never be reused.
	ErrKeyRevoked = errors.New("auth: public key was revoked")
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
