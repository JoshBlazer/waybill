package txmgr

import (
	"errors"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
)

// RevertReason tells a revert apart from every other estimation failure and
// extracts the reason when the node returns one. reverted is false for
// transport errors, timeouts and anything else that only means "could not
// find out".
func RevertReason(err error) (reason string, reverted bool) {
	msg := err.Error()
	var de rpc.DataError
	if errors.As(err, &de) {
		if s, ok := de.ErrorData().(string); ok {
			if data, derr := hexutil.Decode(s); derr == nil && len(data) >= 4 {
				if r, uerr := abi.UnpackRevert(data); uerr == nil {
					return "execution reverted: " + r, true
				}
				return "execution reverted: custom error 0x" + hexutil.Encode(data[:4])[2:], true
			}
		}
	}
	// geth and Anvil report reverts as JSON-RPC errors whose message starts
	// with "execution reverted" (code 3 when data is present).
	var re rpc.Error
	if errors.As(err, &re) && (re.ErrorCode() == 3 || strings.Contains(re.Error(), "execution reverted")) {
		return msg, true
	}
	if strings.Contains(msg, "execution reverted") {
		return msg, true
	}
	return "", false
}
