package signer

import (
	"context"
	"errors"
	"fmt"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
)

// Local signs with a keypair held in this process.
//
// The weakest acceptable arrangement, and only for the operator's own key —
// which sponsors reserves and pays fees and can move nothing else.
//
// It refuses to exist against the public network without an explicit
// acknowledgement. The previous code logged a warning here, and a warning is
// not a control: it is a note for the incident review. A deployment that ships
// with a mainnet passphrase and a seed in its environment should not start.
type Local struct {
	kp      *keypair.Full
	network string
}

// NewLocal returns a signer backed by an in-process seed.
//
// allowMainnet is the acknowledgement. It exists so that turning it on is a
// deliberate act somebody can be asked about, rather than the default state of
// anyone who never read the warning.
func NewLocal(seed, networkPassphrase string, allowMainnet bool) (*Local, error) {
	kp, err := keypair.ParseFull(seed)
	if err != nil {
		return nil, fmt.Errorf("signer: seed is not a valid Stellar secret: %w", err)
	}
	if networkPassphrase == "" {
		return nil, errors.New("signer: local signer needs a network")
	}
	if networkPassphrase == network.PublicNetworkPassphrase && !allowMainnet {
		return nil, fmt.Errorf(
			"%w: this is the public network and the key is in the environment. "+
				"Move signing behind a remote signer, or set the acknowledgement "+
				"explicitly if you mean it", ErrMainnetKeyInProcess)
	}
	return &Local{kp: kp, network: networkPassphrase}, nil
}

// Address reports the account this signs as.
func (l *Local) Address(context.Context) (string, error) { return l.kp.Address(), nil }

// Sign adds this key's signature.
func (l *Local) Sign(_ context.Context, req Request) (Response, error) {
	if req.FeeBump != nil {
		signed, err := req.FeeBump.Sign(l.network, l.kp)
		if err != nil {
			return Response{}, fmt.Errorf("signer: sign fee bump: %w", err)
		}
		return Response{FeeBump: signed, Complete: true}, nil
	}

	signed, err := req.Tx.Sign(l.network, l.kp)
	if err != nil {
		return Response{}, fmt.Errorf("signer: sign transaction: %w", err)
	}
	return Response{Tx: signed, Complete: true}, nil
}
