// Copyright 2020 Coinbase, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"os"
	"path"
	"strconv"
	"strings"

	rosetta "github.com/coinbase/rosetta-sdk-go/gen"

	"github.com/davecgh/go-spew/spew"
)

const (
	// headBlockKey is used to lookup the head block identifier.
	// The head block is the block with the largest index that is
	// not orphaned.
	headBlockKey = "head-block"

	// blockHashNamespace is prepended to any stored block hash.
	// We cannot just use the stored block key to lookup whether
	// a hash has been used before because it is concatenated
	// with the index of the stored block.
	blockHashNamespace = "block-hash"

	// transactionHashNamespace is prepended to any stored
	// transaction hash.
	transactionHashNamespace = "transaction-hash"

	// balanceNamespace is prepended to any stored balance.
	balanceNamespace = "balance"

	// bootstrapBalancesFile is loaded to bootstrap the balance
	// of a collection of accounts.
	bootstrapBalancesFile = "bootstrap_balances.csv"

	// bootstrapBalancesPermissions specifies that the user can
	// read and write the file.
	bootstrapBalancesPermissions = 0600

	// bootstrapBalancesHeader is used as the CSV header
	// in the bootstrapBalancesFile.
	bootstrapBalancesHeader = "AccountIdentifier_address,Amount_value,Currency_symbol,Currency_decimals"
	bootstrapAddressIndex   = 0
	bootstrapValueIndex     = 1
	bootstrapSymbolIndex    = 2
	bootstrapDecimalsIndex  = 3
)

var (
	// ErrHeadBlockNotFound is returned when there is no
	// head block found in BlockStorage.
	ErrHeadBlockNotFound = errors.New("Head block not found")

	// ErrBlockNotFound is returned when a block is not
	// found in BlockStorage.
	ErrBlockNotFound = errors.New("Block not found")

	// ErrAccountNotFound is returned when an account
	// is not found in BlockStorage.
	ErrAccountNotFound = errors.New("Account not found")

	// ErrNegativeBalance is returned when an account
	// balance goes negative as the result of an operation.
	ErrNegativeBalance = errors.New("Negative balance")

	// ErrDuplicateBlockHash is returned when a block hash
	// cannot be stored because it is a duplicate.
	ErrDuplicateBlockHash = errors.New("Duplicate block hash")

	// ErrDuplicateTransactionHash is returned when a transaction
	// hash cannot be stored because it is a duplicate.
	ErrDuplicateTransactionHash = errors.New("Duplicate transaction hash")

	// ErrAlreadyStartedSyncing is returned when trying to bootstrap
	// balances after syncing has started.
	ErrAlreadyStartedSyncing = errors.New("already started syncing")

	// ErrIncorrectHeader is returned when a bootstrap file has an
	// incorrect header.
	ErrIncorrectHeader = errors.New("incorrect header")
)

/*
  Key Construction
*/

// hashBytes is used to construct a SHA1
// hash to protect against arbitrarily
// large key sizes.
func hashBytes(data []byte) []byte {
	h := sha256.New()
	_, err := h.Write(data)
	if err != nil {
		log.Fatal(err)
	}

	return h.Sum(nil)
}

// hashString is used to construct a SHA1
// hash to protect against arbitrarily
// large key sizes.
func hashString(data string) string {
	return fmt.Sprintf("%x", hashBytes([]byte(data)))
}

func getHeadBlockKey() []byte {
	return hashBytes([]byte(headBlockKey))
}

func getBlockKey(blockIdentifier *rosetta.BlockIdentifier) []byte {
	return hashBytes(
		[]byte(fmt.Sprintf("%s:%d", blockIdentifier.Hash, blockIdentifier.Index)),
	)
}

func getHashKey(hash string, isBlock bool) []byte {
	if isBlock {
		return hashBytes([]byte(fmt.Sprintf("%s:%s", blockHashNamespace, hash)))
	}

	return hashBytes([]byte(fmt.Sprintf("%s:%s", transactionHashNamespace, hash)))
}

func getBalanceKey(account *rosetta.AccountIdentifier) []byte {
	if account.SubAccount == nil {
		return hashBytes(
			[]byte(fmt.Sprintf("%s:%s", balanceNamespace, account.Address)),
		)
	}

	if account.SubAccount.Metadata == nil {
		return hashBytes([]byte(fmt.Sprintf(
			"%s:%s:%s",
			balanceNamespace,
			account.Address,
			account.SubAccount.SubAccount,
		)))
	}

	// TODO: handle SubAccount.Metadata
	// that contains pointer values.
	return hashBytes([]byte(fmt.Sprintf(
		"%s:%s:%s:%v",
		balanceNamespace,
		account.Address,
		account.SubAccount.SubAccount,
		*account.SubAccount.Metadata,
	)))
}

// BlockStorage implements block specific storage methods
// on top of a Database and DatabaseTransaction interface.
type BlockStorage struct {
	db Database
}

// NewBlockStorage returns a new BlockStorage.
func NewBlockStorage(ctx context.Context, db Database) *BlockStorage {
	return &BlockStorage{
		db: db,
	}
}

// NewDatabaseTransaction returns a DatabaseTransaction
// from the Database that is backing BlockStorage.
func (b *BlockStorage) NewDatabaseTransaction(
	ctx context.Context,
	write bool,
) DatabaseTransaction {
	return b.db.NewDatabaseTransaction(ctx, write)
}

// GetHeadBlockIdentifier returns the head block identifier,
// if it exists.
func (b *BlockStorage) GetHeadBlockIdentifier(
	ctx context.Context,
	transaction DatabaseTransaction,
) (*rosetta.BlockIdentifier, error) {
	exists, block, err := transaction.Get(ctx, getHeadBlockKey())
	if err != nil {
		return nil, err
	}

	if !exists {
		return nil, ErrHeadBlockNotFound
	}

	dec := gob.NewDecoder(bytes.NewReader(block))
	var blockIdentifier rosetta.BlockIdentifier
	err = dec.Decode(&blockIdentifier)
	if err != nil {
		return nil, err
	}

	return &blockIdentifier, nil
}

// StoreHeadBlockIdentifier stores a block identifier
// or returns an error.
func (b *BlockStorage) StoreHeadBlockIdentifier(
	ctx context.Context,
	transaction DatabaseTransaction,
	blockIdentifier *rosetta.BlockIdentifier,
) error {
	buf := new(bytes.Buffer)
	err := gob.NewEncoder(buf).Encode(blockIdentifier)
	if err != nil {
		return err
	}

	return transaction.Set(ctx, getHeadBlockKey(), buf.Bytes())
}

// GetBlock returns a block, if it exists.
func (b *BlockStorage) GetBlock(
	ctx context.Context,
	transaction DatabaseTransaction,
	blockIdentifier *rosetta.BlockIdentifier,
) (*rosetta.Block, error) {
	exists, block, err := transaction.Get(ctx, getBlockKey(blockIdentifier))
	if err != nil {
		return nil, err
	}

	if !exists {
		return nil, fmt.Errorf("%w %+v", ErrBlockNotFound, blockIdentifier)
	}

	var rosettaBlock rosetta.Block
	err = gob.NewDecoder(bytes.NewBuffer(block)).Decode(&rosettaBlock)
	if err != nil {
		return nil, err
	}

	return &rosettaBlock, nil
}

// storeHash stores either a block or transaction hash.
func (b *BlockStorage) storeHash(
	ctx context.Context,
	transaction DatabaseTransaction,
	hash string,
	isBlock bool,
) error {
	key := getHashKey(hash, isBlock)
	exists, _, err := transaction.Get(ctx, key)
	if err != nil {
		return err
	}

	if !exists {
		return transaction.Set(ctx, key, []byte(""))
	}

	if isBlock {
		return fmt.Errorf(
			"%w %s",
			ErrDuplicateBlockHash,
			hash,
		)
	}

	return fmt.Errorf(
		"%w %s",
		ErrDuplicateTransactionHash,
		hash,
	)
}

// StoreBlock stores a block or returns an error.
// StoreBlock also stores the block hash and all
// its transaction hashes for duplicate detection.
func (b *BlockStorage) StoreBlock(
	ctx context.Context,
	transaction DatabaseTransaction,
	block *rosetta.Block,
) error {
	buf := new(bytes.Buffer)
	err := gob.NewEncoder(buf).Encode(block)
	if err != nil {
		return err
	}

	// Store block
	err = transaction.Set(ctx, getBlockKey(block.BlockIdentifier), buf.Bytes())
	if err != nil {
		return err
	}

	// Store block hash
	err = b.storeHash(ctx, transaction, block.BlockIdentifier.Hash, true)
	if err != nil {
		return err
	}

	// Store all transaction hashes
	for _, txn := range block.Transactions {
		err = b.storeHash(ctx, transaction, txn.TransactionIdentifier.Hash, false)
		if err != nil {
			return err
		}
	}

	return nil
}

// RemoveBlock removes a block or returns an error.
// RemoveBlock also removes the block hash and all
// its transaction hashes to not break duplicate
// detection. This is called within a re-org.
func (b *BlockStorage) RemoveBlock(
	ctx context.Context,
	transaction DatabaseTransaction,
	block *rosetta.BlockIdentifier,
) error {
	// Remove all transaction hashes
	blockData, err := b.GetBlock(ctx, transaction, block)
	for _, txn := range blockData.Transactions {
		err = transaction.Delete(ctx, getHashKey(txn.TransactionIdentifier.Hash, false))
		if err != nil {
			return err
		}
	}

	// Remove block hash
	err = transaction.Delete(ctx, getHashKey(block.Hash, true))
	if err != nil {
		return err
	}

	// Remove block
	return transaction.Delete(ctx, getBlockKey(block))
}

type balanceEntry struct {
	Amounts map[string]*rosetta.Amount
	Block   *rosetta.BlockIdentifier
}

func serializeBalanceEntry(bal balanceEntry) ([]byte, error) {
	buf := new(bytes.Buffer)
	err := gob.NewEncoder(buf).Encode(bal)
	if err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func parseBalanceEntry(buf []byte) (*balanceEntry, error) {
	dec := gob.NewDecoder(bytes.NewReader(buf))
	var bal balanceEntry
	err := dec.Decode(&bal)
	if err != nil {
		return nil, err
	}

	return &bal, nil
}

// GetCurrencyKey is used to identify a *rosetta.Currency
// in an account's map of currencies. It is not feasible
// to create a map of [rosetta.Currency]*rosetta.Amount
// because rosetta.Currency contains a metadata pointer
// that would prevent any equality.
func GetCurrencyKey(currency *rosetta.Currency) string {
	if currency.Metadata == nil {
		return hashString(
			fmt.Sprintf("%s:%d", currency.Symbol, currency.Decimals),
		)
	}

	// TODO: Handle currency.Metadata
	// that has pointer value.
	return hashString(
		fmt.Sprintf(
			"%s:%d:%v",
			currency.Symbol,
			currency.Decimals,
			*currency.Metadata,
		),
	)
}

// BalanceChange represents a balance change that affected
// a *rosetta.AccountIdentifier and a *rosetta.Currency.
type BalanceChange struct {
	Account    *rosetta.AccountIdentifier
	Currency   *rosetta.Currency
	Block      *rosetta.BlockIdentifier
	OldValue   string
	NewValue   string
	Difference string
}

// UpdateBalance updates a rosetta.AccountIdentifer
// by a rosetta.Amount and sets the account's most
// recent accessed block.
func (b *BlockStorage) UpdateBalance(
	ctx context.Context,
	transaction DatabaseTransaction,
	account *rosetta.AccountIdentifier,
	amount *rosetta.Amount,
	block *rosetta.BlockIdentifier,
) (*BalanceChange, error) {
	if amount == nil || amount.Currency == nil {
		return nil, errors.New("invalid amount")
	}

	key := getBalanceKey(account)
	// Get existing balance on key
	exists, balance, err := transaction.Get(ctx, key)
	if err != nil {
		return nil, err
	}

	currencyKey := GetCurrencyKey(amount.Currency)

	if !exists {
		amountMap := make(map[string]*rosetta.Amount)
		newVal, ok := new(big.Int).SetString(amount.Value, 10)
		if !ok {
			return nil, fmt.Errorf("%s is not an integer", amount.Value)
		}

		if newVal.Sign() == -1 {
			return nil, fmt.Errorf(
				"%w %+v for %+v at %+v",
				ErrNegativeBalance,
				spew.Sdump(amount),
				account,
				block,
			)
		}
		amountMap[currencyKey] = amount

		serialBal, err := serializeBalanceEntry(balanceEntry{
			Amounts: amountMap,
			Block:   block,
		})
		if err != nil {
			return nil, err
		}

		if err := transaction.Set(ctx, key, serialBal); err != nil {
			return nil, err
		}

		return &BalanceChange{
			Account:    account,
			Currency:   amount.Currency,
			Block:      block,
			OldValue:   "0",
			NewValue:   amount.Value,
			Difference: amount.Value,
		}, nil
	}

	// Modify balance
	parseBal, err := parseBalanceEntry(balance)
	if err != nil {
		return nil, err
	}

	val, ok := parseBal.Amounts[currencyKey]
	if !ok {
		parseBal.Amounts[currencyKey] = amount
	}

	modification, ok := new(big.Int).SetString(amount.Value, 10)
	if !ok {
		return nil, fmt.Errorf("%s is not an integer", amount.Value)
	}

	existing, ok := new(big.Int).SetString(val.Value, 10)
	if !ok {
		return nil, fmt.Errorf("%s is not an integer", val.Value)
	}

	newVal := new(big.Int).Add(existing, modification)
	val.Value = newVal.String()
	if newVal.Sign() == -1 {
		return nil, fmt.Errorf(
			"%w %+v for %+v at %+v",
			ErrNegativeBalance,
			spew.Sdump(val),
			account,
			block,
		)
	}

	parseBal.Amounts[currencyKey] = val

	parseBal.Block = block
	serialBal, err := serializeBalanceEntry(*parseBal)
	if err != nil {
		return nil, err
	}

	if err := transaction.Set(ctx, key, serialBal); err != nil {
		return nil, err
	}

	return &BalanceChange{
		Account:    account,
		Currency:   amount.Currency,
		Block:      block,
		OldValue:   existing.String(),
		NewValue:   newVal.String(),
		Difference: amount.Value,
	}, nil
}

// GetBalance returns all the balances of a rosetta.AccountIdentifier
// and the rosetta.BlockIdentifier it was last updated at.
func (b *BlockStorage) GetBalance(
	ctx context.Context,
	transaction DatabaseTransaction,
	account *rosetta.AccountIdentifier,
) (map[string]*rosetta.Amount, *rosetta.BlockIdentifier, error) {
	key := getBalanceKey(account)
	exists, bal, err := transaction.Get(ctx, key)
	if err != nil {
		return nil, nil, err
	}

	if !exists {
		return nil, nil, fmt.Errorf("%w %+v", ErrAccountNotFound, account)
	}

	deserialBal, err := parseBalanceEntry(bal)
	if err != nil {
		return nil, nil, err
	}

	return deserialBal.Amounts, deserialBal.Block, nil
}

// BootstrapBalances is utilized to set the balance of
// any number of AccountIdentifiers at the genesis blocks.
// This is particularly useful for setting the value of
// accounts that received an allocation in the genesis block.
func (b *BlockStorage) BootstrapBalances(
	ctx context.Context,
	dataDir string,
	genesisBlockIdentifier *rosetta.BlockIdentifier,
) error {
	f, err := os.OpenFile(
		path.Join(dataDir, bootstrapBalancesFile),
		os.O_RDONLY,
		bootstrapBalancesPermissions,
	)
	if err != nil {
		return err
	}

	dbTransaction := b.NewDatabaseTransaction(ctx, true)
	defer dbTransaction.Discard(ctx)

	_, err = b.GetHeadBlockIdentifier(ctx, dbTransaction)
	if err != ErrHeadBlockNotFound {
		return ErrAlreadyStartedSyncing
	}

	csvReader := csv.NewReader(f)
	rowsRead := 0
	for {
		record, err := csvReader.Read()
		if err == io.EOF {
			break
		}
		rowsRead++

		// Assert header is correct
		if rowsRead == 1 {
			if bootstrapBalancesHeader != strings.Join(record[:], ",") {
				return ErrIncorrectHeader
			}

			continue
		}

		// Assert row column length correct
		if len(record) != len(strings.Split(bootstrapBalancesHeader, ",")) {
			return fmt.Errorf("row %d does not have expected fields: %s", rowsRead, record)
		}

		account := &rosetta.AccountIdentifier{
			Address: record[bootstrapAddressIndex],
		}

		currencyDecimals, err := strconv.Atoi(record[bootstrapDecimalsIndex])
		if err != nil {
			return err
		}

		amount := &rosetta.Amount{
			Value: record[bootstrapValueIndex],
			Currency: &rosetta.Currency{
				Symbol:   record[bootstrapSymbolIndex],
				Decimals: int32(currencyDecimals),
			},
		}

		log.Printf(
			"Setting account %s balance to %s %+v\n",
			account.Address,
			amount.Value,
			amount.Currency,
		)

		_, err = b.UpdateBalance(
			ctx,
			dbTransaction,
			account,
			amount,
			genesisBlockIdentifier,
		)
		if err != nil {
			return err
		}
	}

	err = dbTransaction.Commit(ctx)
	if err != nil {
		return err
	}

	log.Printf("%d Balances Bootstrapped\n", rowsRead-1)
	return nil
}
