// Copyright (c) 2013-2014 Conformal Systems LLC.
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package btcchain

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/conformal/btcdb"
	"github.com/conformal/btcnet"
	"github.com/conformal/btcscript"
	"github.com/conformal/btcutil"
	"github.com/conformal/btcwire"
)

const (
	// MaxSigOpsPerBlock is the maximum number of signature operations
	// allowed for a block.  It is a fraction of the max block payload size.
	MaxSigOpsPerBlock = btcwire.MaxBlockPayload / 50

	// lockTimeThreshold is the number below which a lock time is
	// interpreted to be a block number.  Since an average of one block
	// is generated per 10 minutes, this allows blocks for about 9,512
	// years.  However, if the field is interpreted as a timestamp, given
	// the lock time is a uint32, the max is sometime around 2106.
	lockTimeThreshold uint32 = 5e8 // Tue Nov 5 00:53:20 1985 UTC

	// MaxTimeOffsetSeconds is the maximum number of seconds a block time
	// is allowed to be ahead of the current time.  This is currently 2
	// hours.
	MaxTimeOffsetSeconds = 2 * 60 * 60

	// MinCoinbaseScriptLen is the minimum length a coinbase script can be.
	MinCoinbaseScriptLen = 2

	// MaxCoinbaseScriptLen is the maximum length a coinbase script can be.
	MaxCoinbaseScriptLen = 100

	// medianTimeBlocks is the number of previous blocks which should be
	// used to calculate the median time used to validate block timestamps.
	medianTimeBlocks = 11

	// serializedHeightVersion is the block version which changed block
	// coinbases to start with the serialized block height.
	serializedHeightVersion = 2

	// baseSubsidy is the starting subsidy amount for mined blocks.  This
	// value is halved every SubsidyHalvingInterval blocks.
	baseSubsidy = 50 * btcutil.SatoshiPerBitcoin

	// CoinbaseMaturity is the number of blocks required before newly
	// mined bitcoins (coinbase transactions) can be spent.
	CoinbaseMaturity = 100
)

var (
	// coinbaseMaturity is the internal variable used for validating the
	// spending of coinbase outputs.  A variable rather than the exported
	// constant is used because the tests need the ability to modify it.
	coinbaseMaturity = int64(CoinbaseMaturity)

	// zeroHash is the zero value for a btcwire.ShaHash and is defined as
	// a package level variable to avoid the need to create a new instance
	// every time a check is needed.
	zeroHash = &btcwire.ShaHash{}

	// block91842Hash is one of the two nodes which violate the rules
	// set forth in BIP0030.  It is defined as a package level variable to
	// avoid the need to create a new instance every time a check is needed.
	block91842Hash = newShaHashFromStr("00000000000a4d0a398161ffc163c503763b1f4360639393e0e4c8e300e0caec")

	// block91880Hash is one of the two nodes which violate the rules
	// set forth in BIP0030.  It is defined as a package level variable to
	// avoid the need to create a new instance every time a check is needed.
	block91880Hash = newShaHashFromStr("00000000000743f190a18c5577a3c2d2a1f610ae9601ac046a38084ccb7cd721")
)

// isNullOutpoint determines whether or not a previous transaction output point
// is set.
func isNullOutpoint(outpoint *btcwire.OutPoint) bool {
	if outpoint.Index == math.MaxUint32 && outpoint.Hash.IsEqual(zeroHash) {
		return true
	}
	return false
}

// IsCoinBase determines whether or not a transaction is a coinbase.  A coinbase
// is a special transaction created by miners that has no inputs.  This is
// represented in the block chain by a transaction with a single input that has
// a previous output transaction index set to the maximum value along with a
// zero hash.
func IsCoinBase(tx *btcutil.Tx) bool {
	msgTx := tx.MsgTx()

	// A coin base must only have one transaction input.
	if len(msgTx.TxIn) != 1 {
		return false
	}

	// The previous output of a coin base must have a max value index and
	// a zero hash.
	prevOut := msgTx.TxIn[0].PreviousOutpoint
	if prevOut.Index != math.MaxUint32 || !prevOut.Hash.IsEqual(zeroHash) {
		return false
	}

	return true
}

// IsFinalizedTransaction determines whether or not a transaction is finalized.
func IsFinalizedTransaction(tx *btcutil.Tx, blockHeight int64, blockTime time.Time) bool {
	msgTx := tx.MsgTx()

	// Lock time of zero means the transaction is finalized.
	lockTime := msgTx.LockTime
	if lockTime == 0 {
		return true
	}

	// The lock time field of a transaction is either a block height at
	// which the transaction is finalized or a timestamp depending on if the
	// value is before the lockTimeThreshold.  When it is under the
	// threshold it is a block height.
	blockTimeOrHeight := int64(0)
	if lockTime < lockTimeThreshold {
		blockTimeOrHeight = blockHeight
	} else {
		blockTimeOrHeight = blockTime.Unix()
	}
	if int64(lockTime) < blockTimeOrHeight {
		return true
	}

	// At this point, the transaction's lock time hasn't occured yet, but
	// the transaction might still be finalized if the sequence number
	// for all transaction inputs is maxed out.
	for _, txIn := range msgTx.TxIn {
		if txIn.Sequence != math.MaxUint32 {
			return false
		}
	}
	return true
}

// isBIP0030Node returns whether or not the passed node represents one of the
// two blocks that violate the BIP0030 rule which prevents transactions from
// overwriting old ones.
func isBIP0030Node(node *blockNode) bool {
	if node.height == 91842 && node.hash.IsEqual(block91842Hash) {
		return true
	}

	if node.height == 91880 && node.hash.IsEqual(block91880Hash) {
		return true
	}

	return false
}

// CalcBlockSubsidy returns the subsidy amount a block at the provided height
// should have. This is mainly used for determining how much the coinbase for
// newly generated blocks awards as well as validating the coinbase for blocks
// has the expected value.
//
// The subsidy is halved every SubsidyHalvingInterval blocks.  Mathematically
// this is: baseSubsidy / 2^(height/subsidyHalvingInterval)
//
// At the target block generation rate for the main network, this is
// approximately every 4 years.
func CalcBlockSubsidy(height int64, netParams *btcnet.Params) int64 {
	if netParams.SubsidyHalvingInterval == 0 {
		return baseSubsidy
	}

	// Equivalent to: baseSubsidy / 2^(height/subsidyHalvingInterval)
	return baseSubsidy >> uint(height/int64(netParams.SubsidyHalvingInterval))
}

// CheckTransactionSanity performs some preliminary checks on a transaction to
// ensure it is sane.  These checks are context free.
func CheckTransactionSanity(tx *btcutil.Tx) error {
	// A transaction must have at least one input.
	msgTx := tx.MsgTx()
	if len(msgTx.TxIn) == 0 {
		return ruleError(ErrNoTxInputs, "transaction has no inputs")
	}

	// A transaction must have at least one output.
	if len(msgTx.TxOut) == 0 {
		return ruleError(ErrNoTxOutputs, "transaction has no outputs")
	}

	// A transaction must not exceed the maximum allowed block payload when
	// serialized.
	serializedTxSize := tx.MsgTx().SerializeSize()
	if serializedTxSize > btcwire.MaxBlockPayload {
		str := fmt.Sprintf("serialized transaction is too big - got "+
			"%d, max %d", serializedTxSize, btcwire.MaxBlockPayload)
		return ruleError(ErrTxTooBig, str)
	}

	// Ensure the transaction amounts are in range.  Each transaction
	// output must not be negative or more than the max allowed per
	// transaction.  Also, the total of all outputs must abide by the same
	// restrictions.  All amounts in a transaction are in a unit value known
	// as a satoshi.  One bitcoin is a quantity of satoshi as defined by the
	// SatoshiPerBitcoin constant.
	var totalSatoshi int64
	for _, txOut := range msgTx.TxOut {
		satoshi := txOut.Value
		if satoshi < 0 {
			str := fmt.Sprintf("transaction output has negative "+
				"value of %v", satoshi)
			return ruleError(ErrBadTxOutValue, str)
		}
		if satoshi > btcutil.MaxSatoshi {
			str := fmt.Sprintf("transaction output value of %v is "+
				"higher than max allowed value of %v", satoshi,
				btcutil.MaxSatoshi)
			return ruleError(ErrBadTxOutValue, str)
		}

		// TODO(davec): No need to check < 0 here as satoshi is
		// guaranteed to be positive per the above check.  Also need
		// to add overflow checks.
		totalSatoshi += satoshi
		if totalSatoshi < 0 {
			str := fmt.Sprintf("total value of all transaction "+
				"outputs has negative value of %v", totalSatoshi)
			return ruleError(ErrBadTxOutValue, str)
		}
		if totalSatoshi > btcutil.MaxSatoshi {
			str := fmt.Sprintf("total value of all transaction "+
				"outputs is %v which is higher than max "+
				"allowed value of %v", totalSatoshi,
				btcutil.MaxSatoshi)
			return ruleError(ErrBadTxOutValue, str)
		}
	}

	// Check for duplicate transaction inputs.
	existingTxOut := make(map[btcwire.OutPoint]struct{})
	for _, txIn := range msgTx.TxIn {
		if _, exists := existingTxOut[txIn.PreviousOutpoint]; exists {
			return ruleError(ErrDuplicateTxInputs, "transaction "+
				"contains duplicate inputs")
		}
		existingTxOut[txIn.PreviousOutpoint] = struct{}{}
	}

	// Coinbase script length must be between min and max length.
	if IsCoinBase(tx) {
		slen := len(msgTx.TxIn[0].SignatureScript)
		if slen < MinCoinbaseScriptLen || slen > MaxCoinbaseScriptLen {
			str := fmt.Sprintf("coinbase transaction script length "+
				"of %d is out of range (min: %d, max: %d)",
				slen, MinCoinbaseScriptLen, MaxCoinbaseScriptLen)
			return ruleError(ErrBadCoinbaseScriptLen, str)
		}
	} else {
		// Previous transaction outputs referenced by the inputs to this
		// transaction must not be null.
		for _, txIn := range msgTx.TxIn {
			prevOut := &txIn.PreviousOutpoint
			if isNullOutpoint(prevOut) {
				return ruleError(ErrBadTxInput, "transaction "+
					"input refers to previous output that "+
					"is null")
			}
		}
	}

	return nil
}

// checkProofOfWork ensures the block header bits which indicate the target
// difficulty is in min/max range and that the block hash is less than the
// target difficulty as claimed.
//
//
// The flags modify the behavior of this function as follows:
//  - BFNoPoWCheck: The check to ensure the block hash is less than the target
//    difficulty is not performed.
func checkProofOfWork(block *btcutil.Block, powLimit *big.Int, flags BehaviorFlags) error {
	// The target difficulty must be larger than zero.
	target := CompactToBig(block.MsgBlock().Header.Bits)
	if target.Sign() <= 0 {
		str := fmt.Sprintf("block target difficulty of %064x is too low",
			target)
		return ruleError(ErrUnexpectedDifficulty, str)
	}

	// The target difficulty must be less than the maximum allowed.
	if target.Cmp(powLimit) > 0 {
		str := fmt.Sprintf("block target difficulty of %064x is "+
			"higher than max of %064x", target, powLimit)
		return ruleError(ErrUnexpectedDifficulty, str)
	}

	// The block hash must be less than the claimed target unless the flag
	// to avoid proof of work checks is set.
	if flags&BFNoPoWCheck != BFNoPoWCheck {
		// The block hash must be less than the claimed target.
		blockHash, err := block.Sha()
		if err != nil {
			return err
		}
		hashNum := ShaHashToBig(blockHash)
		if hashNum.Cmp(target) > 0 {
			str := fmt.Sprintf("block hash of %064x is higher than "+
				"expected max of %064x", hashNum, target)
			return ruleError(ErrHighHash, str)
		}
	}

	return nil
}

// CheckProofOfWork ensures the block header bits which indicate the target
// difficulty is in min/max range and that the block hash is less than the
// target difficulty as claimed.
func CheckProofOfWork(block *btcutil.Block, powLimit *big.Int) error {
	return checkProofOfWork(block, powLimit, BFNone)
}

// CountSigOps returns the number of signature operations for all transaction
// input and output scripts in the provided transaction.  This uses the
// quicker, but imprecise, signature operation counting mechanism from
// btcscript.
func CountSigOps(tx *btcutil.Tx) int {
	msgTx := tx.MsgTx()

	// Accumulate the number of signature operations in all transaction
	// inputs.
	totalSigOps := 0
	for _, txIn := range msgTx.TxIn {
		numSigOps := btcscript.GetSigOpCount(txIn.SignatureScript)
		totalSigOps += numSigOps
	}

	// Accumulate the number of signature operations in all transaction
	// outputs.
	for _, txOut := range msgTx.TxOut {
		numSigOps := btcscript.GetSigOpCount(txOut.PkScript)
		totalSigOps += numSigOps
	}

	return totalSigOps
}

// CountP2SHSigOps returns the number of signature operations for all input
// transactions which are of the pay-to-script-hash type.  This uses the
// precise, signature operation counting mechanism from btcscript which requires
// access to the input transaction scripts.
func CountP2SHSigOps(tx *btcutil.Tx, isCoinBaseTx bool, txStore TxStore) (int, error) {
	// Coinbase transactions have no interesting inputs.
	if isCoinBaseTx {
		return 0, nil
	}

	// Accumulate the number of signature operations in all transaction
	// inputs.
	msgTx := tx.MsgTx()
	totalSigOps := 0
	for _, txIn := range msgTx.TxIn {
		// Ensure the referenced input transaction is available.
		txInHash := &txIn.PreviousOutpoint.Hash
		originTx, exists := txStore[*txInHash]
		if !exists || originTx.Err != nil || originTx.Tx == nil {
			str := fmt.Sprintf("unable to find input transaction "+
				"%v referenced from transaction %v", txInHash,
				tx.Sha())
			return 0, ruleError(ErrMissingTx, str)
		}
		originMsgTx := originTx.Tx.MsgTx()

		// Ensure the output index in the referenced transaction is
		// available.
		originTxIndex := txIn.PreviousOutpoint.Index
		if originTxIndex >= uint32(len(originMsgTx.TxOut)) {
			str := fmt.Sprintf("out of bounds input index %d in "+
				"transaction %v referenced from transaction %v",
				originTxIndex, txInHash, tx.Sha())
			return 0, ruleError(ErrBadTxInput, str)
		}

		// We're only interested in pay-to-script-hash types, so skip
		// this input if it's not one.
		pkScript := originMsgTx.TxOut[originTxIndex].PkScript
		if !btcscript.IsPayToScriptHash(pkScript) {
			continue
		}

		// Count the precise number of signature operations in the
		// referenced public key script.
		sigScript := txIn.SignatureScript
		numSigOps := btcscript.GetPreciseSigOpCount(sigScript, pkScript,
			true)

		// We could potentially overflow the accumulator so check for
		// overflow.
		lastSigOps := totalSigOps
		totalSigOps += numSigOps
		if totalSigOps < lastSigOps {
			str := fmt.Sprintf("the public key script from "+
				"output index %d in transaction %v contains "+
				"too many signature operations - overflow",
				originTxIndex, txInHash)
			return 0, ruleError(ErrTooManySigOps, str)
		}
	}

	return totalSigOps, nil
}

// checkBlockSanity performs some preliminary checks on a block to ensure it is
// sane before continuing with block processing.  These checks are context free.
//
// The flags do not modify the behavior of this function directly, however they
// are needed to pass along to checkProofOfWork.
func checkBlockSanity(block *btcutil.Block, powLimit *big.Int, flags BehaviorFlags) error {
	// A block must have at least one transaction.
	msgBlock := block.MsgBlock()
	numTx := len(msgBlock.Transactions)
	if numTx == 0 {
		return ruleError(ErrNoTransactions, "block does not contain "+
			"any transactions")
	}

	// A block must not have more transactions than the max block payload.
	if numTx > btcwire.MaxBlockPayload {
		str := fmt.Sprintf("block contains too many transactions - "+
			"got %d, max %d", numTx, btcwire.MaxBlockPayload)
		return ruleError(ErrTooManyTransactions, str)
	}

	// A block must not exceed the maximum allowed block payload when
	// serialized.
	serializedSize := msgBlock.SerializeSize()
	if serializedSize > btcwire.MaxBlockPayload {
		str := fmt.Sprintf("serialized block is too big - got %d, "+
			"max %d", serializedSize, btcwire.MaxBlockPayload)
		return ruleError(ErrBlockTooBig, str)
	}

	// Ensure the proof of work bits in the block header is in min/max range
	// and the block hash is less than the target value described by the
	// bits.
	err := checkProofOfWork(block, powLimit, flags)
	if err != nil {
		return err
	}

	// A block timestamp must not have a greater precision than one second.
	// This check is necessary because Go time.Time values support
	// nanosecond precision whereas the consensus rules only apply to
	// seconds and it's much nicer to deal with standard Go time values
	// instead of converting to seconds everywhere.
	header := &block.MsgBlock().Header
	if !header.Timestamp.Equal(time.Unix(header.Timestamp.Unix(), 0)) {
		str := fmt.Sprintf("block timestamp of %v has a higher "+
			"precision than one second", header.Timestamp)
		return ruleError(ErrInvalidTime, str)
	}

	// Ensure the block time is not too far in the future.
	maxTimestamp := time.Now().Add(time.Second * MaxTimeOffsetSeconds)
	if header.Timestamp.After(maxTimestamp) {
		str := fmt.Sprintf("block timestamp of %v is too far in the "+
			"future", header.Timestamp)
		return ruleError(ErrTimeTooNew, str)
	}

	// The first transaction in a block must be a coinbase.
	transactions := block.Transactions()
	if !IsCoinBase(transactions[0]) {
		return ruleError(ErrFirstTxNotCoinbase, "first transaction in "+
			"block is not a coinbase")
	}

	// A block must not have more than one coinbase.
	for i, tx := range transactions[1:] {
		if IsCoinBase(tx) {
			str := fmt.Sprintf("block contains second coinbase at "+
				"index %d", i)
			return ruleError(ErrMultipleCoinbases, str)
		}
	}

	// Do some preliminary checks on each transaction to ensure they are
	// sane before continuing.
	for _, tx := range transactions {
		err := CheckTransactionSanity(tx)
		if err != nil {
			return err
		}
	}

	// Build merkle tree and ensure the calculated merkle root matches the
	// entry in the block header.  This also has the effect of caching all
	// of the transaction hashes in the block to speed up future hash
	// checks.  Bitcoind builds the tree here and checks the merkle root
	// after the following checks, but there is no reason not to check the
	// merkle root matches here.
	merkles := BuildMerkleTreeStore(block.Transactions())
	calculatedMerkleRoot := merkles[len(merkles)-1]
	if !header.MerkleRoot.IsEqual(calculatedMerkleRoot) {
		str := fmt.Sprintf("block merkle root is invalid - block "+
			"header indicates %v, but calculated value is %v",
			header.MerkleRoot, calculatedMerkleRoot)
		return ruleError(ErrBadMerkleRoot, str)
	}

	// Check for duplicate transactions.  This check will be fairly quick
	// since the transaction hashes are already cached due to building the
	// merkle tree above.
	existingTxHashes := make(map[btcwire.ShaHash]struct{})
	for _, tx := range transactions {
		hash := tx.Sha()
		if _, exists := existingTxHashes[*hash]; exists {
			str := fmt.Sprintf("block contains duplicate "+
				"transaction %v", hash)
			return ruleError(ErrDuplicateTx, str)
		}
		existingTxHashes[*hash] = struct{}{}
	}

	// The number of signature operations must be less than the maximum
	// allowed per block.
	totalSigOps := 0
	for _, tx := range transactions {
		// We could potentially overflow the accumulator so check for
		// overflow.
		lastSigOps := totalSigOps
		totalSigOps += CountSigOps(tx)
		if totalSigOps < lastSigOps || totalSigOps > MaxSigOpsPerBlock {
			str := fmt.Sprintf("block contains too many signature "+
				"operations - got %v, max %v", totalSigOps,
				MaxSigOpsPerBlock)
			return ruleError(ErrTooManySigOps, str)
		}
	}

	return nil
}

// CheckBlockSanity performs some preliminary checks on a block to ensure it is
// sane before continuing with block processing.  These checks are context free.
func CheckBlockSanity(block *btcutil.Block, powLimit *big.Int) error {
	return checkBlockSanity(block, powLimit, BFNone)
}

// checkSerializedHeight checks if the signature script in the passed
// transaction starts with the serialized block height of wantHeight.
func checkSerializedHeight(coinbaseTx *btcutil.Tx, wantHeight int64) error {
	sigScript := coinbaseTx.MsgTx().TxIn[0].SignatureScript
	if len(sigScript) < 1 {
		str := "the coinbase signature script for blocks of " +
			"version %d or greater must start with the " +
			"length of the serialized block height"
		str = fmt.Sprintf(str, serializedHeightVersion)
		return ruleError(ErrMissingCoinbaseHeight, str)
	}

	serializedLen := int(sigScript[0])
	if len(sigScript[1:]) < serializedLen {
		str := "the coinbase signature script for blocks of " +
			"version %d or greater must start with the " +
			"serialized block height"
		str = fmt.Sprintf(str, serializedLen)
		return ruleError(ErrMissingCoinbaseHeight, str)
	}

	serializedHeightBytes := make([]byte, 8, 8)
	copy(serializedHeightBytes, sigScript[1:serializedLen+1])
	serializedHeight := binary.LittleEndian.Uint64(serializedHeightBytes)
	if int64(serializedHeight) != wantHeight {
		str := fmt.Sprintf("the coinbase signature script serialized "+
			"block height is %d when %d was expected",
			serializedHeight, wantHeight)
		return ruleError(ErrBadCoinbaseHeight, str)
	}

	return nil
}

// isTransactionSpent returns whether or not the provided transaction data
// describes a fully spent transaction.  A fully spent transaction is one where
// all outputs have been spent.
func isTransactionSpent(txD *TxData) bool {
	for _, isOutputSpent := range txD.Spent {
		if !isOutputSpent {
			return false
		}
	}
	return true
}

// checkBIP0030 ensures blocks do not contain duplicate transactions which
// 'overwrite' older transactions that are not fully spent.  This prevents an
// attack where a coinbase and all of its dependent transactions could be
// duplicated to effectively revert the overwritten transactions to a single
// confirmation thereby making them vulnerable to a double spend.
//
// For more details, see https://en.bitcoin.it/wiki/BIP_0030 and
// http://r6.ca/blog/20120206T005236Z.html.
func (b *BlockChain) checkBIP0030(node *blockNode, block *btcutil.Block) error {
	// Attempt to fetch duplicate transactions for all of the transactions
	// in this block from the point of view of the parent node.
	fetchSet := make(map[btcwire.ShaHash]struct{})
	for _, tx := range block.Transactions() {
		fetchSet[*tx.Sha()] = struct{}{}
	}
	txResults, err := b.fetchTxStore(node, fetchSet)
	if err != nil {
		return err
	}

	// Examine the resulting data about the requested transactions.
	for _, txD := range txResults {
		switch txD.Err {
		// A duplicate transaction was not found.  This is the most
		// common case.
		case btcdb.TxShaMissing:
			continue

		// A duplicate transaction was found.  This is only allowed if
		// the duplicate transaction is fully spent.
		case nil:
			if !isTransactionSpent(txD) {
				str := fmt.Sprintf("tried to overwrite "+
					"transaction %v at block height %d "+
					"that is not fully spent", txD.Hash,
					txD.BlockHeight)
				return ruleError(ErrOverwriteTx, str)
			}

		// Some other unexpected error occurred.  Return it now.
		default:
			return txD.Err
		}
	}

	return nil
}

// CheckTransactionInputs performs a series of checks on the inputs to a
// transaction to ensure they are valid.  An example of some of the checks
// include verifying all inputs exist, ensuring the coinbase seasoning
// requirements are met, detecting double spends, validating all values and fees
// are in the legal range and the total output amount doesn't exceed the input
// amount, and verifying the signatures to prove the spender was the owner of
// the bitcoins and therefore allowed to spend them.  As it checks the inputs,
// it also calculates the total fees for the transaction and returns that value.
func CheckTransactionInputs(tx *btcutil.Tx, txHeight int64, txStore TxStore) (int64, error) {
	// Coinbase transactions have no inputs.
	if IsCoinBase(tx) {
		return 0, nil
	}

	txHash := tx.Sha()
	var totalSatoshiIn int64
	for _, txIn := range tx.MsgTx().TxIn {
		// Ensure the input is available.
		txInHash := &txIn.PreviousOutpoint.Hash
		originTx, exists := txStore[*txInHash]
		if !exists || originTx.Err != nil || originTx.Tx == nil {
			str := fmt.Sprintf("unable to find input transaction "+
				"%v for transaction %v", txInHash, txHash)
			return 0, ruleError(ErrMissingTx, str)
		}

		// Ensure the transaction is not spending coins which have not
		// yet reached the required coinbase maturity.
		if IsCoinBase(originTx.Tx) {
			originHeight := originTx.BlockHeight
			blocksSincePrev := txHeight - originHeight
			if blocksSincePrev < coinbaseMaturity {
				str := fmt.Sprintf("tried to spend coinbase "+
					"transaction %v from height %v at "+
					"height %v before required maturity "+
					"of %v blocks", txInHash, originHeight,
					txHeight, coinbaseMaturity)
				return 0, ruleError(ErrImmatureSpend, str)
			}
		}

		// Ensure the transaction is not double spending coins.
		originTxIndex := txIn.PreviousOutpoint.Index
		if originTxIndex >= uint32(len(originTx.Spent)) {
			str := fmt.Sprintf("out of bounds input index %d in "+
				"transaction %v referenced from transaction %v",
				originTxIndex, txInHash, txHash)
			return 0, ruleError(ErrBadTxInput, str)
		}
		if originTx.Spent[originTxIndex] {
			str := fmt.Sprintf("transaction %v tried to double "+
				"spend coins from transaction %v", txHash,
				txInHash)
			return 0, ruleError(ErrDoubleSpend, str)
		}

		// Ensure the transaction amounts are in range.  Each of the
		// output values of the input transactions must not be negative
		// or more than the max allowed per transaction.  All amounts in
		// a transaction are in a unit value known as a satoshi.  One
		// bitcoin is a quantity of satoshi as defined by the
		// SatoshiPerBitcoin constant.
		originTxSatoshi := originTx.Tx.MsgTx().TxOut[originTxIndex].Value
		if originTxSatoshi < 0 {
			str := fmt.Sprintf("transaction output has negative "+
				"value of %v", originTxSatoshi)
			return 0, ruleError(ErrBadTxOutValue, str)
		}
		if originTxSatoshi > btcutil.MaxSatoshi {
			str := fmt.Sprintf("transaction output value of %v is "+
				"higher than max allowed value of %v",
				originTxSatoshi, btcutil.MaxSatoshi)
			return 0, ruleError(ErrBadTxOutValue, str)
		}

		// The total of all outputs must not be more than the max
		// allowed per transaction.  Also, we could potentially overflow
		// the accumulator so check for overflow.
		lastSatoshiIn := totalSatoshiIn
		totalSatoshiIn += originTxSatoshi
		if totalSatoshiIn < lastSatoshiIn ||
			totalSatoshiIn > btcutil.MaxSatoshi {
			str := fmt.Sprintf("total value of all transaction "+
				"inputs is %v which is higher than max "+
				"allowed value of %v", totalSatoshiIn,
				btcutil.MaxSatoshi)
			return 0, ruleError(ErrBadTxOutValue, str)
		}

		// Mark the referenced output as spent.
		originTx.Spent[originTxIndex] = true
	}

	// Calculate the total output amount for this transaction.  It is safe
	// to ignore overflow and out of range errors here because those error
	// conditions would have already been caught by checkTransactionSanity.
	var totalSatoshiOut int64
	for _, txOut := range tx.MsgTx().TxOut {
		totalSatoshiOut += txOut.Value
	}

	// Ensure the transaction does not spend more than its inputs.
	if totalSatoshiIn < totalSatoshiOut {
		str := fmt.Sprintf("total value of all transaction inputs for "+
			"transaction %v is %v which is less than the amount "+
			"spent of %v", txHash, totalSatoshiIn, totalSatoshiOut)
		return 0, ruleError(ErrSpendTooHigh, str)
	}

	// NOTE: bitcoind checks if the transaction fees are < 0 here, but that
	// is an impossible condition because of the check above that ensures
	// the inputs are >= the outputs.
	txFeeInSatoshi := totalSatoshiIn - totalSatoshiOut
	return txFeeInSatoshi, nil
}

// checkConnectBlock performs several checks to confirm connecting the passed
// block to the main chain (including whatever reorganization might be necessary
// to get this node to the main chain) does not violate any rules.
//
// The CheckConnectBlock function makes use of this function to perform the
// bulk of its work.  The only difference is this function accepts a node which
// may or may not require reorganization to connect it to the main chain whereas
// CheckConnectBlock creates a new node which specifically connects to the end
// of the current main chain and then calls this function with that node.
//
// See the comments for CheckConnectBlock for some examples of the type of
// checks performed by this function.
func (b *BlockChain) checkConnectBlock(node *blockNode, block *btcutil.Block) error {
	// If the side chain blocks end up in the database, a call to
	// CheckBlockSanity should be done here in case a previous version
	// allowed a block that is no longer valid.  However, since the
	// implementation only currently uses memory for the side chain blocks,
	// it isn't currently necessary.

	// The coinbase for the Genesis block is not spendable, so just return
	// now.
	if node.hash.IsEqual(b.netParams.GenesisHash) && b.bestChain == nil {
		return nil
	}

	// BIP0030 added a rule to prevent blocks which contain duplicate
	// transactions that 'overwrite' older transactions which are not fully
	// spent.  See the documentation for checkBIP0030 for more details.
	//
	// There are two blocks in the chain which violate this
	// rule, so the check must be skipped for those blocks. The
	// isBIP0030Node function is used to determine if this block is one
	// of the two blocks that must be skipped.
	enforceBIP0030 := !isBIP0030Node(node)
	if enforceBIP0030 {
		err := b.checkBIP0030(node, block)
		if err != nil {
			return err
		}
	}

	// Request a map that contains all input transactions for the block from
	// the point of view of its position within the block chain.  These
	// transactions are needed for verification of things such as
	// transaction inputs, counting pay-to-script-hashes, and scripts.
	txInputStore, err := b.fetchInputTransactions(node, block)
	if err != nil {
		return err
	}

	// BIP0016 describes a pay-to-script-hash type that is considered a
	// "standard" type.  The rules for this BIP only apply to transactions
	// after the timestamp defined by btcscript.Bip16Activation.  See
	// https://en.bitcoin.it/wiki/BIP_0016 for more details.
	enforceBIP0016 := false
	if node.timestamp.After(btcscript.Bip16Activation) {
		enforceBIP0016 = true
	}

	// The number of signature operations must be less than the maximum
	// allowed per block.  Note that the preliminary sanity checks on a
	// block also include a check similar to this one, but this check
	// expands the count to include a precise count of pay-to-script-hash
	// signature operations in each of the input transaction public key
	// scripts.
	transactions := block.Transactions()
	totalSigOps := 0
	for i, tx := range transactions {
		numsigOps := CountSigOps(tx)
		if enforceBIP0016 {
			// Since the first (and only the first) transaction has
			// already been verified to be a coinbase transaction,
			// use i == 0 as an optimization for the flag to
			// countP2SHSigOps for whether or not the transaction is
			// a coinbase transaction rather than having to do a
			// full coinbase check again.
			numP2SHSigOps, err := CountP2SHSigOps(tx, i == 0,
				txInputStore)
			if err != nil {
				return err
			}
			numsigOps += numP2SHSigOps
		}

		// Check for overflow or going over the limits.  We have to do
		// this on every loop iteration to avoid overflow.
		lastSigops := totalSigOps
		totalSigOps += numsigOps
		if totalSigOps < lastSigops || totalSigOps > MaxSigOpsPerBlock {
			str := fmt.Sprintf("block contains too many "+
				"signature operations - got %v, max %v",
				totalSigOps, MaxSigOpsPerBlock)
			return ruleError(ErrTooManySigOps, str)
		}
	}

	// Perform several checks on the inputs for each transaction.  Also
	// accumulate the total fees.  This could technically be combined with
	// the loop above instead of running another loop over the transactions,
	// but by separating it we can avoid running the more expensive (though
	// still relatively cheap as compared to running the scripts) checks
	// against all the inputs when the signature operations are out of
	// bounds.
	var totalFees int64
	for _, tx := range transactions {
		txFee, err := CheckTransactionInputs(tx, node.height, txInputStore)
		if err != nil {
			return err
		}

		// Sum the total fees and ensure we don't overflow the
		// accumulator.
		lastTotalFees := totalFees
		totalFees += txFee
		if totalFees < lastTotalFees {
			return ruleError(ErrBadFees, "total fees for block "+
				"overflows accumulator")
		}
	}

	// The total output values of the coinbase transaction must not exceed
	// the expected subsidy value plus total transaction fees gained from
	// mining the block.  It is safe to ignore overflow and out of range
	// errors here because those error conditions would have already been
	// caught by checkTransactionSanity.
	var totalSatoshiOut int64
	for _, txOut := range transactions[0].MsgTx().TxOut {
		totalSatoshiOut += txOut.Value
	}
	expectedSatoshiOut := CalcBlockSubsidy(node.height, b.netParams) +
		totalFees
	if totalSatoshiOut > expectedSatoshiOut {
		str := fmt.Sprintf("coinbase transaction for block pays %v "+
			"which is more than expected value of %v",
			totalSatoshiOut, expectedSatoshiOut)
		return ruleError(ErrBadCoinbaseValue, str)
	}

	// Don't run scripts if this node is before the latest known good
	// checkpoint since the validity is verified via the checkpoints (all
	// transactions are included in the merkle root hash and any changes
	// will therefore be detected by the next checkpoint).  This is a huge
	// optimization because running the scripts is the most time consuming
	// portion of block handling.
	checkpoint := b.LatestCheckpoint()
	runScripts := !b.noVerify
	if checkpoint != nil && node.height <= checkpoint.Height {
		runScripts = false
	}

	// Now that the inexpensive checks are done and have passed, verify the
	// transactions are actually allowed to spend the coins by running the
	// expensive ECDSA signature check scripts.  Doing this last helps
	// prevent CPU exhaustion attacks.
	if runScripts {
		err := checkBlockScripts(block, txInputStore)
		if err != nil {
			return err
		}
	}

	return nil
}

// CheckConnectBlock performs several checks to confirm connecting the passed
// block to the main chain does not violate any rules.  An example of some of
// the checks performed are ensuring connecting the block would not cause any
// duplicate transaction hashes for old transactions that aren't already fully
// spent, double spends, exceeding the maximum allowed signature operations
// per block, invalid values in relation to the expected block subisidy, or
// fail transaction script validation.
//
// This function is NOT safe for concurrent access.
func (b *BlockChain) CheckConnectBlock(block *btcutil.Block) error {
	prevNode := b.bestChain
	blockSha, _ := block.Sha()
	newNode := newBlockNode(&block.MsgBlock().Header, blockSha, block.Height())
	if prevNode != nil {
		newNode.parent = prevNode
		newNode.workSum.Add(prevNode.workSum, newNode.workSum)
	}

	return b.checkConnectBlock(newNode, block)
}
