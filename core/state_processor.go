// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package core

import (
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/misc"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"math/big"
	"strings"
)

// StateProcessor is a basic Processor, which takes care of transitioning
// state from one point to another.
//
// StateProcessor implements Processor.
type StateProcessor struct {
	config *params.ChainConfig // Chain configuration options
	bc     *BlockChain         // Canonical block chain
	engine consensus.Engine    // Consensus engine used for block rewards
}

// NewStateProcessor initialises a new StateProcessor.
func NewStateProcessor(config *params.ChainConfig, bc *BlockChain, engine consensus.Engine) *StateProcessor {
	return &StateProcessor{
		config: config,
		bc:     bc,
		engine: engine,
	}
}

// Process processes the state changes according to the Ethereum rules by running
// the transaction messages using the statedb and applying any rewards to both
// the processor (coinbase) and any included uncles.
//
// Process returns the receipts and logs accumulated during the process and
// returns the amount of gas that was used in the process. If any of the
// transactions failed to execute due to insufficient gas it will return an error.
func (p *StateProcessor) Process(block *types.Block, statedb *state.StateDB, cfg vm.Config) (types.Receipts, []*types.Log, uint64, error) {
	var (
		receipts    types.Receipts
		usedGas     = new(uint64)
		header      = block.Header()
		blockHash   = block.Hash()
		blockNumber = block.Number()
		allLogs     []*types.Log
		gp          = new(GasPool).AddGas(block.GasLimit())
	)
	// Mutate the block and state according to any hard-fork specs
	if p.config.DAOForkSupport && p.config.DAOForkBlock != nil && p.config.DAOForkBlock.Cmp(block.Number()) == 0 {
		misc.ApplyDAOHardFork(statedb)
	}
	var (
		context = NewEVMBlockContext(header, p.bc, nil)
		vmenv   = vm.NewEVM(context, vm.TxContext{}, statedb, p.config, cfg)
		signer  = types.MakeSigner(p.config, header.Number, header.Time)
	)
	if beaconRoot := block.BeaconRoot(); beaconRoot != nil {
		ProcessBeaconBlockRoot(*beaconRoot, vmenv, statedb)
	}

	// TODO: this is not correct in general as AA transactions can be anywhere in a block
	verifiedAATransactions := make([]*ValidationPhaseResult, 0)
	for i, tx := range block.Transactions() {
		if tx.Type() == types.ALEXF_AA_TX_TYPE {
			statedb.SetTxContext(tx.Hash(), i) // todo: 'i' is not correct as well if other transactions are in a block!
			vpr, err := ApplyAlexfAATransactionValidationPhase(p.config, p.bc, &header.Coinbase, gp, statedb, header, tx, cfg)
			if err != nil {
				return nil, nil, 0, err
			}
			verifiedAATransactions = append(verifiedAATransactions, vpr)
		}
	}
	for i, vpr := range verifiedAATransactions {

		// TODO: this will miss all validation phase events - pass in 'vpr'
		statedb.SetTxContext(vpr.Tx.Hash(), i)

		receipt, err := ApplyAlexfAATransactionExecutionPhase(p.config, vpr, blockNumber, blockHash, p.bc, &header.Coinbase, gp, statedb, header, cfg)
		if err != nil {
			return nil, nil, 0, err
		}
		receipts = append(receipts, receipt)
		allLogs = append(allLogs, receipt.Logs...)
	}
	// Iterate over and process the individual transactions
	for i, tx := range block.Transactions() {
		if tx.Type() == types.ALEXF_AA_TX_TYPE {
			continue
		}
		msg, err := TransactionToMessage(tx, signer, header.BaseFee)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("could not apply tx %d [%v]: %w", i, tx.Hash().Hex(), err)
		}
		statedb.SetTxContext(tx.Hash(), i)
		receipt, err := applyTransaction(msg, p.config, gp, statedb, blockNumber, blockHash, tx, usedGas, vmenv)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("could not apply tx %d [%v]: %w", i, tx.Hash().Hex(), err)
		}
		receipts = append(receipts, receipt)
		allLogs = append(allLogs, receipt.Logs...)
	}
	// Fail if Shanghai not enabled and len(withdrawals) is non-zero.
	withdrawals := block.Withdrawals()
	if len(withdrawals) > 0 && !p.config.IsShanghai(block.Number(), block.Time()) {
		return nil, nil, 0, errors.New("withdrawals before shanghai")
	}
	// Finalize the block, applying any consensus engine specific extras (e.g. block rewards)
	p.engine.Finalize(p.bc, header, statedb, block.Transactions(), block.Uncles(), withdrawals)

	return receipts, allLogs, *usedGas, nil
}

func applyAlexfAATransactionValidationPhase(aatx *types.AlexfAccountAbstractionTx, thash common.Hash, evm *vm.EVM, gp *GasPool) (*ValidationPhaseResult, error) {
	jsondata := `[
	{"type":"function","name":"validateTransaction","inputs": [{"name": "version","type": "uint256"},{"name": "txHash","type": "bytes32"},{"name": "transaction","type": "bytes"}]}
	]`

	validateTransactionAbi, err := abi.JSON(strings.NewReader(jsondata))

	entryPoint := common.HexToAddress("0x7560000000000000000000000000000000007560")
	println("Alexf EP:", entryPoint.String())
	// TODO: pre-deployed Nonce Manager; this is just a way to pass it in
	var nonceManager common.Address = [20]byte(aatx.PaymasterData[20:40])
	nonceManagerData := make([]byte, 0)
	key := make([]byte, 40) // todo: also nonce
	nonceManagerData = append(nonceManagerData[:], aatx.Sender.Bytes()...)
	nonceManagerData = append(nonceManagerData[:], key...)
	nonceManagerMsg := &Message{
		From:              entryPoint,
		To:                &nonceManager,
		Value:             big.NewInt(0),
		GasLimit:          100000,
		GasPrice:          big.NewInt(875000000),
		GasFeeCap:         big.NewInt(875000000),
		GasTipCap:         big.NewInt(875000000),
		Data:              nonceManagerData,
		AccessList:        aatx.AccessList,
		SkipAccountChecks: true,
		IsInnerAATxFrame:  true,
	}
	resultNonceManager, err := ApplyAATxMessage(evm, nonceManagerMsg, gp)
	if err != nil { // failed due to EntryPoint address not having gas money...
		return nil, err
	}
	fmt.Printf("ALEXF AA resultNonceManager: %s\n", hex.EncodeToString(resultNonceManager.ReturnData))

	if len(aatx.DeployerData) >= 20 {
		var deployerAddress common.Address = [20]byte(aatx.DeployerData[0:20])
		if (deployerAddress.Cmp(common.Address{}) != 0) {
			deployerMsg := &Message{}
			resultDeployer, err := ApplyAATxMessage(evm, deployerMsg, gp)
			if err != nil {
				return nil, err
			}
			fmt.Printf("ALEXF AA resultDeployer: %s", hex.EncodeToString(resultDeployer.ReturnData))
		}
	}

	txAbiEncoding, err := aatx.AbiEncode()
	validateTransactionData, err := validateTransactionAbi.Pack("validateTransaction", big.NewInt(0), thash, txAbiEncoding)
	// selector: bf45c166
	// params:  uint256 version, bytes32 txHash, bytes transaction
	//validateTransactionData, err := common.ParseHexOrString("0xbf45c16600000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000006000000000000000000000000000000000000000000000000000000000000000010000000000000000000000000000000000000000000000000000000000000000")
	//validateTransactionData := make([]byte, 0)
	accountValidationMsg := &Message{
		From:              *aatx.Sender,
		To:                aatx.Sender,
		Value:             big.NewInt(0),
		GasLimit:          100000,
		GasPrice:          big.NewInt(875000000),
		GasFeeCap:         big.NewInt(875000000),
		GasTipCap:         big.NewInt(875000000),
		Data:              validateTransactionData,
		AccessList:        aatx.AccessList,
		SkipAccountChecks: true,
		IsInnerAATxFrame:  true,
	}
	resultAccountValidation, err := ApplyAATxMessage(evm, accountValidationMsg, gp)
	if err != nil {
		return nil, err
	}
	fmt.Printf("\nALEXF AA resultAccountValidation: %s\n", hex.EncodeToString(resultAccountValidation.ReturnData))

	var paymasterContext []byte
	if len(aatx.PaymasterData) >= 20 {
		//0000000000000000000000000000000000000000000000000000000000000000
		// selector: 0xe0e6183a
		// params:  uint256 version, bytes32 txHash, bytes transaction
		//data, err := common.ParseHexOrString("0xe0e6183a")
		data, err := common.ParseHexOrString("0xe0e6183a00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000006000000000000000000000000000000000000000000000000000000000000000010000000000000000000000000000000000000000000000000000000000000000")
		if err != nil {
			return nil, err
		}

		var paymasterAddress common.Address = [20]byte(aatx.PaymasterData[0:20])
		paymasterMsg := &Message{
			From:      *aatx.Sender,
			To:        &paymasterAddress,
			Value:     big.NewInt(0),
			GasLimit:  100000,
			GasPrice:  big.NewInt(875000000),
			GasFeeCap: big.NewInt(875000000),
			GasTipCap: big.NewInt(875000000),
			Data:      data,
			//Data:              aatx.PaymasterData[20:],
			AccessList:        aatx.AccessList,
			SkipAccountChecks: true,
			IsInnerAATxFrame:  true,
		}

		// Apply the Paymaster call frame transaction to the current state (included in the env).
		resultPm, err := ApplyAATxMessage(evm, paymasterMsg, gp)
		if err != nil {
			return nil, err
		}

		if resultPm.Failed() {
			log.Error("ALEXF AA: paymaster validation failed")
			return nil, errors.New("paymaster validation failed - invalid transaction")
		}
		fmt.Printf("\nALEXF AA resultPaymasterValidation: %s\n", hex.EncodeToString(resultPm.ReturnData))
		paymasterContext = resultPm.ReturnData
	}

	vpr := &ValidationPhaseResult{
		paymasterContext:  paymasterContext,
		Thash:             thash,
		validationGasUsed: 0,
		paymasterGasUsed:  0,
	}

	return vpr, nil
}

func applyAlexfAATransactionExecutionPhase(vpr *ValidationPhaseResult, evm *vm.EVM, statedb *state.StateDB, gp *GasPool, blockNumber *big.Int, blockHash common.Hash) (*types.Receipt, error) {
	aatx := vpr.Tx.AlexfAATransactionData()

	accountExecutionMsg := &Message{
		From:              *aatx.Sender,
		To:                aatx.Sender,
		Value:             big.NewInt(0),
		GasLimit:          100000,
		GasPrice:          big.NewInt(875000000),
		GasFeeCap:         big.NewInt(875000000),
		GasTipCap:         big.NewInt(875000000),
		Data:              aatx.Data,
		AccessList:        aatx.AccessList,
		SkipAccountChecks: true,
		IsInnerAATxFrame:  true,
	}
	// TODO: snapshot EVM - we will fall back here if postOp fails
	// / FAILS as msg.From is 0x000 because it is read from the signature
	// Apply the execution call frame transaction to the current state
	result, err := ApplyAATxMessage(evm, accountExecutionMsg, gp)
	if err != nil {
		return nil, err
	}

	if len(vpr.paymasterContext) != 0 {
		postOpData := common.FromHex("0x34a4a77c00000000000000000000000000000000000000000000000000000000000000010000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000006000000000000000000000000000000000000000000000000000000000000000010000000000000000000000000000000000000000000000000000000000000000")
		var paymasterAddress common.Address = [20]byte(aatx.PaymasterData[0:20])
		paymasterPostOpMsg := &Message{
			From:              *aatx.Sender,
			To:                &paymasterAddress,
			Value:             big.NewInt(0),
			GasLimit:          100000,
			GasPrice:          big.NewInt(875000000),
			GasFeeCap:         big.NewInt(875000000),
			GasTipCap:         big.NewInt(875000000),
			Data:              postOpData,
			AccessList:        aatx.AccessList,
			SkipAccountChecks: true,
			IsInnerAATxFrame:  true}
		resultPostOp, err := ApplyAATxMessage(evm, paymasterPostOpMsg, gp)
		if err != nil {
			return nil, err
		}
		fmt.Printf("ALEXF AA resultPostOp: %s", hex.EncodeToString(resultPostOp.ReturnData))
	}

	var root []byte
	receipt := &types.Receipt{Type: vpr.Tx.Type(), PostState: root, CumulativeGasUsed: 0 /**TODO: usedGas*/}

	// Set the receipt logs and create the bloom filter.
	receipt.Logs = statedb.GetLogs(vpr.Tx.Hash(), blockNumber.Uint64(), blockHash)

	if result.Failed() {
		receipt.Status = types.ReceiptStatusFailed
	} else {
		receipt.Status = types.ReceiptStatusSuccessful
	}
	return receipt, err
}

func applyTransaction(msg *Message, config *params.ChainConfig, gp *GasPool, statedb *state.StateDB, blockNumber *big.Int, blockHash common.Hash, tx *types.Transaction, usedGas *uint64, evm *vm.EVM) (*types.Receipt, error) {
	// Create a new context to be used in the EVM environment.
	txContext := NewEVMTxContext(msg)
	evm.Reset(txContext, statedb)

	// Apply the transaction to the current state (included in the env).
	result, err := ApplyMessage(evm, msg, gp)
	if err != nil {
		return nil, err
	}

	// Update the state with pending changes.
	var root []byte
	if config.IsByzantium(blockNumber) {
		statedb.Finalise(true)
	} else {
		root = statedb.IntermediateRoot(config.IsEIP158(blockNumber)).Bytes()
	}
	*usedGas += result.UsedGas

	// Create a new receipt for the transaction, storing the intermediate root and gas used
	// by the tx.
	receipt := &types.Receipt{Type: tx.Type(), PostState: root, CumulativeGasUsed: *usedGas}
	if result.Failed() {
		receipt.Status = types.ReceiptStatusFailed
	} else {
		receipt.Status = types.ReceiptStatusSuccessful
	}
	receipt.TxHash = tx.Hash()
	receipt.GasUsed = result.UsedGas

	if tx.Type() == types.BlobTxType {
		receipt.BlobGasUsed = uint64(len(tx.BlobHashes()) * params.BlobTxBlobGasPerBlob)
		receipt.BlobGasPrice = evm.Context.BlobBaseFee
	}

	// If the transaction created a contract, store the creation address in the receipt.
	if msg.To == nil {
		receipt.ContractAddress = crypto.CreateAddress(evm.TxContext.Origin, tx.Nonce())
	}

	// Set the receipt logs and create the bloom filter.
	receipt.Logs = statedb.GetLogs(tx.Hash(), blockNumber.Uint64(), blockHash)
	receipt.Bloom = types.CreateBloom(types.Receipts{receipt})
	receipt.BlockHash = blockHash
	receipt.BlockNumber = blockNumber
	receipt.TransactionIndex = uint(statedb.TxIndex())
	return receipt, err
}

// ApplyTransaction attempts to apply a transaction to the given state database
// and uses the input parameters for its environment. It returns the receipt
// for the transaction, gas used and an error if the transaction failed,
// indicating the block was invalid.
func ApplyTransaction(config *params.ChainConfig, bc ChainContext, author *common.Address, gp *GasPool, statedb *state.StateDB, header *types.Header, tx *types.Transaction, usedGas *uint64, cfg vm.Config) (*types.Receipt, error) {
	msg, err := TransactionToMessage(tx, types.MakeSigner(config, header.Number, header.Time), header.BaseFee)
	if err != nil {
		return nil, err
	}
	// Create a new context to be used in the EVM environment
	blockContext := NewEVMBlockContext(header, bc, author)
	txContext := NewEVMTxContext(msg)
	vmenv := vm.NewEVM(blockContext, txContext, statedb, config, cfg)
	return applyTransaction(msg, config, gp, statedb, header.Number, header.Hash(), tx, usedGas, vmenv)
}

type ValidationPhaseResult struct {
	Tx                *types.Transaction
	Thash             common.Hash
	paymasterContext  []byte
	validationGasUsed uint64
	paymasterGasUsed  uint64
}

func ApplyAlexfAATransactionValidationPhase(config *params.ChainConfig, bc ChainContext, author *common.Address, gp *GasPool, statedb *state.StateDB, header *types.Header, tx *types.Transaction, cfg vm.Config) (*ValidationPhaseResult, error) {
	log.Error("ALEXF: applying transaction validation phase")
	thash := tx.Hash()
	log.Error(thash.Hex())
	aatx := tx.AlexfAATransactionData()

	blockContext := NewEVMBlockContext(header, bc, author)
	message, err := TransactionToMessage(tx, types.MakeSigner(config, header.Number, header.Time), header.BaseFee)
	txContext := NewEVMTxContext(message)
	vmenv := vm.NewEVM(blockContext, txContext, statedb, config, cfg)
	vmenv.Reset(txContext, statedb) // TODO what does this 'reset' do?

	// Validation phase
	vpr, err := applyAlexfAATransactionValidationPhase(aatx, thash, vmenv, gp)
	if err != nil {
		return nil, err
	}

	vpr.Tx = tx

	return vpr, nil
}

func ApplyAlexfAATransactionExecutionPhase(config *params.ChainConfig, vpr *ValidationPhaseResult, blockNumber *big.Int, blockHash common.Hash, bc ChainContext, author *common.Address, gp *GasPool, statedb *state.StateDB, header *types.Header, cfg vm.Config) (*types.Receipt, error) {
	log.Error("ALEXF: applying transaction execution phase")
	log.Error(vpr.Tx.Hash().Hex())

	// todo: this code is duplicated with validation phase and maybe we need to keep something instead of recreating
	blockContext := NewEVMBlockContext(header, bc, author)
	message, err := TransactionToMessage(vpr.Tx, types.MakeSigner(config, header.Number, header.Time), header.BaseFee)
	txContext := NewEVMTxContext(message)
	vmenv := vm.NewEVM(blockContext, txContext, statedb, config, cfg)
	vmenv.Reset(txContext, statedb) // TODO what does this 'reset' do?
	if err != nil {
		return nil, err
	}

	return applyAlexfAATransactionExecutionPhase(vpr, vmenv, statedb, gp, blockNumber, blockHash)
}

// ProcessBeaconBlockRoot applies the EIP-4788 system call to the beacon block root
// contract. This method is exported to be used in tests.
func ProcessBeaconBlockRoot(beaconRoot common.Hash, vmenv *vm.EVM, statedb *state.StateDB) {
	// If EIP-4788 is enabled, we need to invoke the beaconroot storage contract with
	// the new root
	msg := &Message{
		From:      params.SystemAddress,
		GasLimit:  30_000_000,
		GasPrice:  common.Big0,
		GasFeeCap: common.Big0,
		GasTipCap: common.Big0,
		To:        &params.BeaconRootsStorageAddress,
		Data:      beaconRoot[:],
	}
	vmenv.Reset(NewEVMTxContext(msg), statedb)
	statedb.AddAddressToAccessList(params.BeaconRootsStorageAddress)
	_, _, _ = vmenv.Call(vm.AccountRef(msg.From), *msg.To, msg.Data, 30_000_000, common.Big0)
	statedb.Finalise(true)
}
