// Package evm holds EVM-specific logic that does not need a network:
// deposit-address derivation and address formatting.
package evm

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/google/uuid"
)

// SaltForInvoice is the CREATE2 salt of an invoice's deposit address:
// keccak256 of the invoice UUID's 16 bytes. ForwarderFactory uses the same
// salt, so the address is the same on every network where the factory and
// implementation are deployed at the same addresses.
func SaltForInvoice(id uuid.UUID) [32]byte {
	return crypto.Keccak256Hash(id[:])
}

// cloneInitCode is the EIP-1167 minimal-proxy creation code that
// OpenZeppelin's Clones.cloneDeterministic deploys, with the implementation
// address spliced in after byte 20.
func cloneInitCode(implementation common.Address) []byte {
	code := make([]byte, 0, 55)
	code = append(code, common.FromHex("0x3d602d80600a3d3981f3363d3d373d3d3d363d73")...)
	code = append(code, implementation.Bytes()...)
	code = append(code, common.FromHex("0x5af43d82803e903d91602b57fd5bf3")...)
	return code
}

// PredictDepositAddress returns the address ForwarderFactory.predict(salt)
// returns: CREATE2 by the factory, of a minimal proxy to implementation.
// It must match the contract exactly; TestPredictDepositAddress pins it to
// values read from the deployed factory.
func PredictDepositAddress(factory, implementation common.Address, salt [32]byte) common.Address {
	return crypto.CreateAddress2(factory, salt, crypto.Keccak256(cloneInitCode(implementation)))
}

// Checksum returns the EIP-55 form of a hex address.
func Checksum(hex string) string { return common.HexToAddress(hex).Hex() }

// NormalizeAddress returns the lower-case hex form stored in the database.
func NormalizeAddress(a common.Address) string {
	return "0x" + common.Bytes2Hex(a.Bytes())
}
