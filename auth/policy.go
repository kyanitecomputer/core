// SPDX-License-Identifier: BSD-3-Clause

// Package auth is the Kyanite authorization plane: the single audited decision
// point that governs every NATS connection on a node (in-process IPC, the
// node-to-node data plane, and the human/API access plane), per the NATS auth
// callout architecture.
//
// This file is the authorization policy compiler (architecture §10): subjects
// are the policy language, and capabilities are the vocabulary. A capability
// (for example telemetry.publish or scree.rw) names a fixed set of publish and
// subscribe subject templates. A [PolicyBuilder] collects capability
// definitions and [PolicyBuilder.Build] lowers them into an immutable [Policy] —
// a flat table, in the same Builder→Build discipline as fsm and supervise — so
// minting a principal's permissions is a table lookup plus per-principal
// substitution, never string formatting on the decision path.
//
// The compiler is deliberately stdlib-only: it produces a neutral [Permissions]
// value (allow/deny subject lists), and the callout's minting step converts that
// into a signed NATS user JWT. Keeping policy free of the JWT and NKey libraries
// makes it independently testable and keeps the authorization model separable
// from the token format.
package auth

import (
	"fmt"
	"strings"
)

// Capability names a grant in the policy vocabulary. Capabilities are declared
// by the supervision-tree spec (LOCAL actors) or role tables (OPER principals)
// and compiled to subject permissions here.
type Capability string

// Permissions is a neutral publish/subscribe allow/deny set. It is converted to
// a signed user JWT by the minting step; on its own it carries no token-format
// dependency.
type Permissions struct {
	PubAllow []string
	PubDeny  []string
	SubAllow []string
	SubDeny  []string
}

// Principal is the subject-substitution context for compiling permissions. The
// placeholders {domain}, {device}, {principal}, and {inbox} in capability
// templates are replaced with the corresponding fields.
type Principal struct {
	// Name identifies the principal (used for {principal} and audit).
	Name string
	// Domain is the management domain (used for {domain}).
	Domain string
	// Device is the device identity (used for {device}).
	Device string
	// InboxPrefix is the principal's unique reply-inbox prefix; it is granted
	// subscribe access automatically so replies are isolated per principal.
	InboxPrefix string
	// Capabilities are the grants this principal holds.
	Capabilities []Capability
}

// sysSubject is the system-account subject space, denied to all non-SYS
// principals when DenySystem is set.
const sysSubject = "$SYS.>"

// capBuild accumulates one capability's templates during building.
type capBuild struct {
	pubAllow []string
	pubDeny  []string
	subAllow []string
	subDeny  []string
}

// PolicyBuilder collects capability definitions, then lowers them with Build.
type PolicyBuilder struct {
	caps    map[Capability]*capBuild
	denySys bool
}

// NewPolicy returns an empty PolicyBuilder.
func NewPolicy() *PolicyBuilder {
	return &PolicyBuilder{caps: make(map[Capability]*capBuild)}
}

// DenySystem adds a systematic deny of $SYS.> to every compiled principal — the
// one deny used by default (§10). SYS principals are configured out of band and
// do not go through this compiler.
func (b *PolicyBuilder) DenySystem() *PolicyBuilder {
	b.denySys = true
	return b
}

// CapabilitySpec configures one capability's subject templates.
type CapabilitySpec struct {
	c *capBuild
}

// Capability begins (or resumes) configuration of capability id.
func (b *PolicyBuilder) Capability(id Capability) *CapabilitySpec {
	c := b.caps[id]
	if c == nil {
		c = &capBuild{}
		b.caps[id] = c
	}
	return &CapabilitySpec{c: c}
}

// Publish grants publish access to the given subject templates.
func (s *CapabilitySpec) Publish(subjects ...string) *CapabilitySpec {
	s.c.pubAllow = append(s.c.pubAllow, subjects...)
	return s
}

// Subscribe grants subscribe access to the given subject templates.
func (s *CapabilitySpec) Subscribe(subjects ...string) *CapabilitySpec {
	s.c.subAllow = append(s.c.subAllow, subjects...)
	return s
}

// DenyPublish denies publish access to the given subject templates.
func (s *CapabilitySpec) DenyPublish(subjects ...string) *CapabilitySpec {
	s.c.pubDeny = append(s.c.pubDeny, subjects...)
	return s
}

// DenySubscribe denies subscribe access to the given subject templates.
func (s *CapabilitySpec) DenySubscribe(subjects ...string) *CapabilitySpec {
	s.c.subDeny = append(s.c.subDeny, subjects...)
	return s
}

// Policy is an immutable, compiled capability table. Compile it for a principal
// to produce that principal's permissions. It is safe to share read-only.
type Policy struct {
	caps    map[Capability]capBuild
	denySys bool
}

// Build validates the configuration and lowers it into an immutable Policy. It
// reports a [PolicyBuildError] for empty subject templates.
func (b *PolicyBuilder) Build() (*Policy, error) {
	var issues []string
	caps := make(map[Capability]capBuild, len(b.caps))
	for id, c := range b.caps {
		for _, group := range [][]string{c.pubAllow, c.pubDeny, c.subAllow, c.subDeny} {
			for _, subj := range group {
				if strings.TrimSpace(subj) == "" {
					issues = append(issues, fmt.Sprintf("capability %q has an empty subject", id))
				}
			}
		}
		caps[id] = *c
	}
	if len(issues) > 0 {
		return nil, &PolicyBuildError{Issues: issues}
	}
	return &Policy{caps: caps, denySys: b.denySys}, nil
}

// Compile produces the permissions for a principal by concatenating the subject
// templates of its capabilities with placeholders substituted, adding the
// principal's private inbox to the subscribe allow list, and applying the
// systematic $SYS.> deny when configured. It reports an
// [UnknownCapabilityError] if the principal holds a capability the policy does
// not define.
func (p *Policy) Compile(pr Principal) (Permissions, error) {
	r := strings.NewReplacer(
		"{domain}", pr.Domain,
		"{device}", pr.Device,
		"{principal}", pr.Name,
		"{inbox}", pr.InboxPrefix,
	)
	var perm Permissions
	for _, id := range pr.Capabilities {
		c, ok := p.caps[id]
		if !ok {
			return Permissions{}, &UnknownCapabilityError{Capability: id}
		}
		perm.PubAllow = appendSubst(perm.PubAllow, r, c.pubAllow)
		perm.PubDeny = appendSubst(perm.PubDeny, r, c.pubDeny)
		perm.SubAllow = appendSubst(perm.SubAllow, r, c.subAllow)
		perm.SubDeny = appendSubst(perm.SubDeny, r, c.subDeny)
	}
	if pr.InboxPrefix != "" {
		perm.SubAllow = appendUnique(perm.SubAllow, pr.InboxPrefix+".>")
	}
	if p.denySys {
		perm.PubDeny = appendUnique(perm.PubDeny, sysSubject)
		perm.SubDeny = appendUnique(perm.SubDeny, sysSubject)
	}
	return perm, nil
}

// appendSubst substitutes placeholders in each template and appends unique
// results to dst, preserving order.
func appendSubst(dst []string, r *strings.Replacer, templates []string) []string {
	for _, t := range templates {
		dst = appendUnique(dst, r.Replace(t))
	}
	return dst
}

// appendUnique appends s to dst only if not already present.
func appendUnique(dst []string, s string) []string {
	for _, e := range dst {
		if e == s {
			return dst
		}
	}
	return append(dst, s)
}
