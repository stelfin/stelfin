package settlement

import (
	"fmt"
	"strconv"

	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// Describing a contract call.
//
// The hardest thing to show honestly on this deployment, because a contract
// call is opaque by construction: the network does not know what `transfer`
// means, and neither does stelfin. What can be shown exactly is which contract,
// which function, which arguments, and who else is being asked to authorise —
// so that is what is shown, in full, with nothing summarised away.
//
// What deliberately is NOT shown is an interpretation. There is no attempt to
// recognise a token contract and render "send 5 USDC"; that guess would be
// right most of the time and catastrophic the once it was not, and a person who
// reads "send 5 USDC" and signs something else has been lied to by us rather
// than by the contract.

// describeInvoke renders an InvokeHostFunction.
func describeInvoke(o *txnbuild.InvokeHostFunction) ([]Field, string, error) {
	switch o.HostFunction.Type {
	case xdr.HostFunctionTypeHostFunctionTypeInvokeContract:
		return describeContractCall(o)

	default:
		// Uploading WASM and creating contracts are deployment acts, not
		// treasury operations, and they are not reachable from chat. Refused
		// rather than half-rendered so that the day they are reachable, this
		// stops the envelope rather than waving it through.
		return nil, "", fmt.Errorf("%w: a host function of type %s cannot be shown",
			ErrIndescribable, o.HostFunction.Type.String())
	}
}

func describeContractCall(o *txnbuild.InvokeHostFunction) ([]Field, string, error) {
	args := o.HostFunction.InvokeContract
	if args == nil {
		return nil, "", fmt.Errorf("%w: the contract call carries no arguments block",
			ErrIndescribable)
	}

	contract, err := scAddressString(args.ContractAddress)
	if err != nil {
		return nil, "", err
	}
	function := string(args.FunctionName)

	fields := []Field{
		{Label: "contract", Kind: KindAddress, Value: contract},
		{Label: "function", Kind: KindText, Value: function},
		// The count is its own field so a renderer cannot show three arguments
		// out of four and have the description still look complete.
		{Label: "arguments", Kind: KindNumber, Value: strconv.Itoa(len(args.Args))},
	}

	for i, arg := range args.Args {
		rendered, argErr := scvalString(arg)
		if argErr != nil {
			return nil, "", fmt.Errorf("argument %d: %w", i, argErr)
		}
		fields = append(fields, Field{
			Label: "arg " + strconv.Itoa(i),
			// Raw rather than text: these are type-tagged machine values and a
			// page that styled them as prose would be inviting someone to read
			// them as prose.
			Kind:  KindRaw,
			Value: rendered,
		})
	}

	// Who else is being asked to authorise.
	//
	// Left out, a contract call that moves a third party's tokens would look
	// like a call that moves nothing — the authorisation entries are the part
	// that makes it possible, and they are inside the envelope being signed.
	fields = append(fields, Field{
		Label: "authorizations", Kind: KindNumber, Value: strconv.Itoa(len(o.Auth)),
	})
	for i, entry := range o.Auth {
		who, authErr := authorizerString(entry)
		if authErr != nil {
			return nil, "", fmt.Errorf("authorization %d: %w", i, authErr)
		}
		fields = append(fields, Field{
			Label: "authorized by " + strconv.Itoa(i), Kind: KindAddress, Value: who,
		})
	}

	summary := fmt.Sprintf("Call %s on contract %s with %d argument(s)",
		function, contract, len(args.Args))
	return fields, summary, nil
}

// authorizerString names who an authorisation entry speaks for.
func authorizerString(entry xdr.SorobanAuthorizationEntry) (string, error) {
	switch entry.Credentials.Type {
	case xdr.SorobanCredentialsTypeSorobanCredentialsSourceAccount:
		// The transaction's own source authorises it, which the envelope
		// already says. Named explicitly so the count and the list agree.
		return "the transaction's source account", nil

	case xdr.SorobanCredentialsTypeSorobanCredentialsAddress:
		if entry.Credentials.Address == nil {
			return "", fmt.Errorf("%w: an address credential with no address", ErrIndescribable)
		}
		return scAddressString(entry.Credentials.Address.Address)

	default:
		return "", fmt.Errorf("%w: a credential of type %s cannot be shown",
			ErrIndescribable, entry.Credentials.Type.String())
	}
}
