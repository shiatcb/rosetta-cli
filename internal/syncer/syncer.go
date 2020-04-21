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

package syncer

import (
	"context"
	"fmt"
	"log"
	"math/big"

	"github.com/coinbase/rosetta-validator/internal/storage"

	"github.com/coinbase/rosetta-sdk-go/asserter"
	"github.com/coinbase/rosetta-sdk-go/types"
)

const (
	// maxSync is the maximum number of blocks
	// to try and sync in a given SyncCycle.
	maxSync = 1000
)

// Syncer defines an interface for syncing some
// range of blocks.
type Syncer interface {
	SetStartIndex(
		ctx context.Context,
		startIndex int64,
	) error

	NextSyncableRange(
		ctx context.Context,
		endIndex int64,
	) (
		rangeStart int64,
		rangeEnd int64,
		halt bool,
		err error,
	)

	SyncRange(
		ctx context.Context,
		rangeStart int64,
		rangeEnd int64,
	) error
}

// Sync cycles endlessly until there is an error
// or the requested range is synced.
func Sync(
	ctx context.Context,
	cancel context.CancelFunc,
	s Syncer,
	startIndex int64,
	endIndex int64,
) error {
	defer cancel()

	if err := s.SetStartIndex(ctx, startIndex); err != nil {
		return err
	}

	for {
		rangeStart, rangeEnd, halt, err := s.NextSyncableRange(
			ctx,
			endIndex,
		)
		if err != nil {
			return err
		}
		if halt {
			break
		}

		if rangeEnd-rangeStart > maxSync {
			rangeEnd = rangeStart + maxSync
		}

		log.Printf("Syncing %d-%d\n", rangeStart, rangeEnd)

		err = s.SyncRange(ctx, rangeStart, rangeEnd)
		if err != nil {
			return err
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}
	}

	log.Printf("Finished syncing %d-%d\n", startIndex, endIndex)
	return nil
}

// Handler is called at various times during the sync cycle
// to handle different events. It is common to write logs or
// perform reconciliation in the sync handler.
type Handler interface {
	BlockProcessed(
		ctx context.Context,
		block *types.Block,
		orphan bool,
		changes []*storage.BalanceChange,
	) error
}

// BalanceChanges returns all balance changes for
// a particular block. All balance changes for a
// particular account are summed into a single
// storage.BalanceChanges struct. If a block is being
// orphaned, the opposite of each balance change is
// returned.
func BalanceChanges(
	ctx context.Context,
	asserter *asserter.Asserter,
	block *types.Block,
	orphan bool,
) ([]*storage.BalanceChange, error) {
	balanceChanges := map[string]*storage.BalanceChange{}
	for _, tx := range block.Transactions {
		for _, op := range tx.Operations {
			successful, err := asserter.OperationSuccessful(op)
			if err != nil {
				// Should only occur if responses not validated
				return nil, err
			}

			if !successful {
				continue
			}

			if op.Account == nil {
				continue
			}

			amount := op.Amount
			blockIdentifier := block.BlockIdentifier
			if orphan {
				existing, ok := new(big.Int).SetString(amount.Value, 10)
				if !ok {
					return nil, fmt.Errorf("%s is not an integer", amount.Value)
				}

				amount.Value = new(big.Int).Neg(existing).String()
				blockIdentifier = block.ParentBlockIdentifier
			}

			// Merge values by account and currency
			key := fmt.Sprintf("%s:%s",
				storage.GetBalanceKey(op.Account),
				storage.GetCurrencyKey(op.Amount.Currency),
			)

			val, ok := balanceChanges[key]
			if !ok {
				balanceChanges[key] = &storage.BalanceChange{
					Account:    op.Account,
					Currency:   op.Amount.Currency,
					Difference: amount.Value,
					Block:      blockIdentifier,
				}
				continue
			}

			newDifference, err := storage.AddStringValues(val.Difference, amount.Value)
			if err != nil {
				return nil, err
			}
			val.Difference = newDifference
			balanceChanges[key] = val
		}
	}

	allChanges := []*storage.BalanceChange{}
	for _, change := range balanceChanges {
		allChanges = append(allChanges, change)
	}

	return allChanges, nil
}
