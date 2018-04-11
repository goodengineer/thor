package genesis

import (
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/vechain/thor/builtin"
	"github.com/vechain/thor/state"
	"github.com/vechain/thor/thor"
	"github.com/vechain/thor/tx"
	"github.com/vechain/thor/vm/evm"
)

type account struct {
	Address    thor.Address
	PrivateKey *ecdsa.PrivateKey
}

var devAccounts atomic.Value

func DevAccounts() []account {
	if accs := devAccounts.Load(); accs != nil {
		return accs.([]account)
	}

	var accs []account
	privKeys := []string{
		"dce1443bd2ef0c2631adc1c67e5c93f13dc23a41c18b536effbbdcbcdb96fb65",
		"321d6443bc6177273b5abf54210fe806d451d6b7973bccc2384ef78bbcd0bf51",
		"2d7c882bad2a01105e36dda3646693bc1aaaa45b0ed63fb0ce23c060294f3af2",
		"593537225b037191d322c3b1df585fb1e5100811b71a6f7fc7e29cca1333483e",
		"ca7b25fc980c759df5f3ce17a3d881d6e19a38e651fc4315fc08917edab41058",
		"88d2d80b12b92feaa0da6d62309463d20408157723f2d7e799b6a74ead9a673b",
		"fbb9e7ba5fe9969a71c6599052237b91adeb1e5fc0c96727b66e56ff5d02f9d0",
		"547fb081e73dc2e22b4aae5c60e2970b008ac4fc3073aebc27d41ace9c4f53e9",
		"c8c53657e41a8d669349fc287f57457bd746cb1fcfc38cf94d235deb2cfca81b",
		"87e0eba9c86c494d98353800571089f316740b0cb84c9a7cdf2fe5c9997c7966",
	}
	for _, str := range privKeys {
		pk, err := crypto.HexToECDSA(str)
		if err != nil {
			panic(err)
		}
		addr := crypto.PubkeyToAddress(pk.PublicKey)
		accs = append(accs, account{thor.Address(addr), pk})
	}
	devAccounts.Store(accs)
	return accs
}

func NewDevnet() (*Genesis, error) {
	launchTime := uint64(1519356186)

	builder := new(Builder).
		GasLimit(thor.InitialGasLimit).
		Timestamp(launchTime).
		State(func(state *state.State) error {
			// alloc precompiled contracts
			for addr := range evm.PrecompiledContractsByzantium {
				state.SetCode(thor.Address(addr), emptyRuntimeBytecode)
			}
			state.SetCode(builtin.Authority.Address, builtin.Authority.RuntimeBytecodes())
			state.SetCode(builtin.Energy.Address, builtin.Energy.RuntimeBytecodes())
			state.SetCode(builtin.Params.Address, builtin.Params.RuntimeBytecodes())

			energy := builtin.Energy.WithState(state)
			tokenSupply := &big.Int{}
			for _, a := range DevAccounts() {
				b, _ := new(big.Int).SetString("10000000000000000000000000", 10)
				state.SetBalance(a.Address, b)
				tokenSupply.Add(tokenSupply, b)
				energy.AddBalance(0, a.Address, b)
			}
			energy.InitializeTokenSupply(tokenSupply)
			return nil
		}).
		Call(
			tx.NewClause(&builtin.Params.Address).
				WithData(mustEncodeInput(builtin.Params.ABI, "set", thor.KeyRewardRatio, thor.InitialRewardRatio)),
			builtin.Executor.Address).
		Call(
			tx.NewClause(&builtin.Params.Address).
				WithData(mustEncodeInput(builtin.Params.ABI, "set", thor.KeyBaseGasPrice, thor.InitialBaseGasPrice)),
			builtin.Executor.Address).
		Call(
			tx.NewClause(&builtin.Params.Address).
				WithData(mustEncodeInput(builtin.Params.ABI, "set", thor.KeyProposerEndorsement, thor.InitialProposerEndorsement)),
			builtin.Executor.Address)

	for i, a := range DevAccounts() {
		builder.Call(
			tx.NewClause(&builtin.Authority.Address).
				WithData(mustEncodeInput(builtin.Authority.ABI, "add", a.Address, a.Address, thor.BytesToBytes32([]byte(fmt.Sprintf("a%v", i))))),
			builtin.Executor.Address)
	}

	id, err := builder.ComputeID()
	if err != nil {
		return nil, err
	}

	return &Genesis{builder, id}, nil
}