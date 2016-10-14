package validation

import (
	"bytes"
	"context"
	"encoding/hex"
	"runtime"
	"strings"

	"golang.org/x/sync/errgroup"

	"chain/errors"
	"chain/protocol/bc"
	"chain/protocol/state"
	"chain/protocol/vm"
	"chain/protocol/vmutil"
)

// Errors returned by ValidateAndApplyBlock
var (
	ErrBadPrevHash  = errors.New("invalid previous block hash")
	ErrBadHeight    = errors.New("invalid block height")
	ErrBadTimestamp = errors.New("invalid block timestamp")
	ErrBadScript    = errors.New("unspendable block script")
	ErrBadSig       = errors.New("invalid signature script")
	ErrBadTxRoot    = errors.New("invalid transaction merkle root")
	ErrBadStateRoot = errors.New("invalid state merkle root")
)

// ValidateAndApplyBlock validates the given block against the given
// state tree and applies its changes to the state snapshot.
// If block is invalid, it returns a non-nil error describing why.
func ValidateAndApplyBlock(ctx context.Context, snapshot *state.Snapshot, prevBlock, block *bc.Block, validateTx func(*bc.Tx) error) error {
	return validateBlock(ctx, snapshot, prevBlock, block, validateTx, true)
}

// ValidateBlockForSig performs validation on an incoming _unsigned_
// block in preparation for signing it.  By definition it does not
// execute the sigscript. It also uses a disposable copy of the
// supplied snapshot.
func ValidateBlockForSig(ctx context.Context, snapshot *state.Snapshot, prevBlock, block *bc.Block, validateTx func(*bc.Tx) error) error {
	return validateBlock(ctx, state.Copy(snapshot), prevBlock, block, validateTx, false)
}

func validateBlock(ctx context.Context, snapshot *state.Snapshot, prevBlock, block *bc.Block, validateTx func(*bc.Tx) error, runScript bool) error {

	var g errgroup.Group
	// Do all of the unparallelizable work, plus validating the block
	// header in one goroutine.
	g.Go(func() error {
		var prev *bc.BlockHeader
		if prevBlock != nil {
			prev = &prevBlock.BlockHeader
		}
		err := validateBlockHeader(prev, block, runScript)
		if err != nil {
			return err
		}
		snapshot.PruneIssuances(block.TimestampMS)

		// TODO: Check that other block headers are valid.
		// TODO(erykwalder): consider writing to a copy of the state tree
		// of the one provided and make the caller call ApplyBlock as well
		for _, tx := range block.Transactions {
			err = ConfirmTx(snapshot, block, tx)
			if err != nil {
				return err
			}
			err = ApplyTx(snapshot, tx)
			if err != nil {
				return err
			}
		}
		if block.AssetsMerkleRoot != snapshot.Tree.RootHash() {
			return ErrBadStateRoot
		}
		return nil
	})

	// Distribute checking well-formedness of the transactions across
	// GOMAXPROCS goroutines.
	ch := make(chan *bc.Tx, len(block.Transactions))
	for i := 0; i < runtime.GOMAXPROCS(0); i++ {
		g.Go(func() error {
			for tx := range ch {
				if err := validateTx(tx); err != nil {
					return err
				}
			}
			return nil
		})
	}
	for _, tx := range block.Transactions {
		ch <- tx
	}
	close(ch)
	return g.Wait()
}

// ApplyBlock applies the transactions in the block to the state tree.
func ApplyBlock(snapshot *state.Snapshot, block *bc.Block) error {
	snapshot.PruneIssuances(block.TimestampMS)
	for _, tx := range block.Transactions {
		err := ApplyTx(snapshot, tx)
		if err != nil {
			return err
		}
	}
	return nil
}

func validateBlockHeader(prev *bc.BlockHeader, block *bc.Block, runScript bool) error {
	if prev == nil && block.Height != 1 {
		return ErrBadHeight
	}
	if prev != nil {
		prevHash := prev.Hash()
		if !bytes.Equal(block.PreviousBlockHash[:], prevHash[:]) {
			return ErrBadPrevHash
		}
		if block.Height != prev.Height+1 {
			return ErrBadHeight
		}
		if block.TimestampMS < prev.TimestampMS {
			return ErrBadTimestamp
		}
	}

	txMerkleRoot := CalcMerkleRoot(block.Transactions)
	// can be modified to allow soft fork
	if block.TransactionsMerkleRoot != txMerkleRoot {
		return ErrBadTxRoot
	}

	if vmutil.IsUnspendable(block.ConsensusProgram) {
		return ErrBadScript
	}

	if runScript && prev != nil {
		ok, err := vm.VerifyBlockHeader(prev, block)
		if err == nil && !ok {
			err = ErrFalseVMResult
		}
		if err != nil {
			pkScriptStr, _ := vm.Disassemble(prev.ConsensusProgram)
			witnessStrs := make([]string, 0, len(block.Witness))
			for _, w := range block.Witness {
				witnessStrs = append(witnessStrs, hex.EncodeToString(w))
			}
			witnessStr := strings.Join(witnessStrs, "; ")
			return errors.Wrapf(ErrBadSig, "validation failed in script execution in block (program [%s] witness [%s]): %s", pkScriptStr, witnessStr, err)
		}
	}

	return nil
}
