package deployments

import (
	"context"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/JoshBlazer/waybill/api/internal/safety"
)

// Addresses from a real Deploy.s.sol run on a fresh Anvil (see evm tests).
var good = Deployment{
	Network:        "evm:31337",
	ChainID:        31337,
	Token:          common.HexToAddress("0x5FbDB2315678afecb367f032d93F642f64180aa3"),
	Vault:          common.HexToAddress("0xe7f1725E7734CE288F8367e1Bb143E90bb3F0512"),
	Factory:        common.HexToAddress("0x9fE46736679d2D9a65F0992F2272dE9f3c7fa6e0"),
	Implementation: common.HexToAddress("0x75537828f2ce51be7289709686A69CbFDbB714F1"),
}

// fakeChain answers the three calls Verify makes, like a correct chain
// would, unless told to lie.
type fakeChain struct {
	impl     common.Address
	predict  func(salt []byte) common.Address
	decimals int64
	noCode   bool
}

func (f fakeChain) CallContract(_ context.Context, msg ethereum.CallMsg, _ *big.Int) ([]byte, error) {
	if f.noCode {
		return nil, nil
	}
	sel := string(msg.Data[:4])
	switch sel {
	case string(crypto.Keccak256([]byte("implementation()"))[:4]):
		return common.LeftPadBytes(f.impl.Bytes(), 32), nil
	case string(crypto.Keccak256([]byte("predict(bytes32)"))[:4]):
		return common.LeftPadBytes(f.predict(msg.Data[4:]).Bytes(), 32), nil
	case string(crypto.Keccak256([]byte("decimals()"))[:4]):
		return common.LeftPadBytes(big.NewInt(f.decimals).Bytes(), 32), nil
	}
	return nil, errors.New("unexpected call")
}

func honest() fakeChain {
	return fakeChain{
		impl:     good.Implementation,
		predict:  func(s []byte) common.Address { return good.PredictDepositAddress([32]byte(s)) },
		decimals: 6,
	}
}

func TestVerify_AcceptsMatchingChain(t *testing.T) {
	if err := Verify(context.Background(), honest(), good); err != nil {
		t.Fatalf("Verify = %v", err)
	}
}

func TestVerify_RejectsMismatches(t *testing.T) {
	wrongImpl := honest()
	wrongImpl.impl = common.HexToAddress("0x1")
	wrongPredict := honest()
	wrongPredict.predict = func([]byte) common.Address { return common.HexToAddress("0xdead") }
	wrongDecimals := honest()
	wrongDecimals.decimals = 18
	noCode := honest()
	noCode.noCode = true

	for name, c := range map[string]fakeChain{
		"factory reports another implementation": wrongImpl,
		"factory predicts a different address":   wrongPredict,
		"token is not six decimals":              wrongDecimals,
		"nothing deployed at the address":        noCode,
	} {
		if err := Verify(context.Background(), c, good); err == nil {
			t.Errorf("%s: Verify accepted it", name)
		}
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("31337.json", `{"chainId":31337,"token":"0x5FbDB2315678afecb367f032d93F642f64180aa3","vault":"0xe7f1725E7734CE288F8367e1Bb143E90bb3F0512","factory":"0x9fE46736679d2D9a65F0992F2272dE9f3c7fa6e0","implementation":"0x75537828f2ce51be7289709686A69CbFDbB714F1"}`)
	got, err := Load(dir, []safety.Network{"evm:31337", "btc:regtest"})
	if err != nil {
		t.Fatal(err)
	}
	if d := got["evm:31337"]; d.Factory != good.Factory || d.Network != "evm:31337" {
		t.Fatalf("Load = %+v", d)
	}

	write("84532.json", `{"chainId":31337,"token":"0x5FbDB2315678afecb367f032d93F642f64180aa3","vault":"0xe7f1725E7734CE288F8367e1Bb143E90bb3F0512","factory":"0x9fE46736679d2D9a65F0992F2272dE9f3c7fa6e0","implementation":"0x75537828f2ce51be7289709686A69CbFDbB714F1"}`)
	if _, err := Load(dir, []safety.Network{"evm:84532"}); err == nil || !strings.Contains(err.Error(), "records chain id 31337") {
		t.Fatalf("Load accepted a file for the wrong chain: %v", err)
	}
	if _, err := Load(dir, []safety.Network{"evm:11155111"}); err == nil {
		t.Fatal("Load accepted a missing file")
	}
}
