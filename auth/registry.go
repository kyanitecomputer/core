// SPDX-License-Identifier: BSD-3-Clause

package auth

// LocalRegistry maps per-boot public NKeys to LOCAL principals (architecture
// §7). At boot the runtime generates one ephemeral Ed25519 NKey per declared
// child that needs a connection, registers its public key and capability set
// here (before the child starts), and hands the seed to the child; the callout
// then resolves a connecting LOCAL actor by looking its public key up here.
//
// The registry enforces restart hygiene: revoking a key removes its mapping and
// tombstones the key value so it can never be reused, so a wedged-then-restarted
// child cannot leave a zombie authenticated connection with stale permissions
// (the caller kicks the revoked connection separately).
//
// It is single-owner — driven from the auth actor goroutine — and holds no
// locks, matching the zero-mutex discipline of supervise and fsm.
type LocalRegistry struct {
	byKey   map[string]Principal
	revoked map[string]struct{}
}

// NewLocalRegistry returns an empty registry.
func NewLocalRegistry() *LocalRegistry {
	return &LocalRegistry{
		byKey:   make(map[string]Principal),
		revoked: make(map[string]struct{}),
	}
}

// Register binds publicKey to principal pr. It returns [ErrEmptyKey] for an
// empty key, [ErrKeyRevoked] if the key value was revoked (reuse is forbidden),
// or [ErrKeyRegistered] if the key is already registered (rotate by revoking
// first).
func (r *LocalRegistry) Register(publicKey string, pr Principal) error {
	if publicKey == "" {
		return ErrEmptyKey
	}
	if _, ok := r.revoked[publicKey]; ok {
		return ErrKeyRevoked
	}
	if _, ok := r.byKey[publicKey]; ok {
		return ErrKeyRegistered
	}
	r.byKey[publicKey] = pr
	return nil
}

// Lookup returns the principal registered for publicKey.
func (r *LocalRegistry) Lookup(publicKey string) (Principal, bool) {
	pr, ok := r.byKey[publicKey]
	return pr, ok
}

// Revoke removes publicKey's registration and tombstones the key value. It
// returns the principal that was registered (for the caller to kick its
// connection) and whether a registration existed.
func (r *LocalRegistry) Revoke(publicKey string) (Principal, bool) {
	pr, ok := r.byKey[publicKey]
	if ok {
		delete(r.byKey, publicKey)
	}
	if publicKey != "" {
		r.revoked[publicKey] = struct{}{}
	}
	return pr, ok
}

// Revoked reports whether publicKey has been tombstoned.
func (r *LocalRegistry) Revoked(publicKey string) bool {
	_, ok := r.revoked[publicKey]
	return ok
}

// Len returns the number of currently registered (non-revoked) principals.
func (r *LocalRegistry) Len() int { return len(r.byKey) }
