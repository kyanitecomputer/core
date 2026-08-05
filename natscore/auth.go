// SPDX-License-Identifier: BSD-3-Clause

package natscore

import (
	"fmt"

	"github.com/nats-io/nats-server/v2/server"
)

// Account names for the fixed five-account layout (auth architecture §5): SYS
// for system/monitoring, AUTH for the callout service, LOCAL for in-process
// actors, OPER for human/API sessions, and MESH for node-to-node subjects.
const (
	AccountSystem = "SYS"
	AccountAuth   = "AUTH"
	AccountLocal  = "LOCAL"
	AccountOper   = "OPER"
	AccountMesh   = "MESH"
)

// AuthCallout identifies the callout service and issuer for [EnableAuthCallout].
// All fields are public keys; the corresponding seeds live in the auth plane
// (and, in production, a TEE).
type AuthCallout struct {
	// IssuerPublicKey is the account NKey (A...) whose signature the server
	// trusts on minted user JWTs and authorization responses.
	IssuerPublicKey string
	// ServicePublicKey is the callout service's user NKey (U...). It is placed
	// in the AUTH account's auth_users bypass so the service itself connects
	// without being subject to the callout.
	ServicePublicKey string
	// XKeyPublicKey, when set, enables XKey encryption of callout requests and
	// responses. Empty disables encryption (bring-up only).
	XKeyPublicKey string
}

// EnableAuthCallout augments opts with the five-account layout, a system
// account, and an auth_callout block that delegates client authorization to the
// service identified by ac.ServicePublicKey. The service connects with the
// bypass key; every other client connection — including in-process LOCAL actors
// — is governed by the callout. Enable this only once the callout service is
// ready to answer, since enabling it rejects all non-bypass connects otherwise.
func EnableAuthCallout(opts *server.Options, ac AuthCallout) error {
	if ac.IssuerPublicKey == "" || ac.ServicePublicKey == "" {
		return fmt.Errorf("natscore: auth callout requires issuer and service public keys")
	}

	authAcc := server.NewAccount(AccountAuth)
	opts.Accounts = append(opts.Accounts,
		server.NewAccount(AccountSystem),
		authAcc,
		server.NewAccount(AccountLocal),
		server.NewAccount(AccountOper),
		server.NewAccount(AccountMesh),
	)
	opts.SystemAccount = AccountSystem

	// The callout service's bypass identity lives in the AUTH account.
	opts.Nkeys = append(opts.Nkeys, &server.NkeyUser{
		Nkey:    ac.ServicePublicKey,
		Account: authAcc,
	})

	opts.AuthCallout = &server.AuthCallout{
		Issuer:    ac.IssuerPublicKey,
		Account:   AccountAuth,
		AuthUsers: []string{ac.ServicePublicKey},
		XKey:      ac.XKeyPublicKey,
	}
	return nil
}
