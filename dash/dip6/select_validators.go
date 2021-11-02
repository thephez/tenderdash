package dip6

import (
	"fmt"
	"math"

	tmbytes "github.com/tendermint/tendermint/libs/bytes"
	"github.com/tendermint/tendermint/types"
)

// SelectValidatorsDIP6 selects validators from the `validatorSetMembers`, based on algorithm
// described in DIP-6 https://github.com/dashpay/dips/blob/master/dip-0006.md
func SelectValidatorsDIP6(
	validatorSetMembers []*types.Validator,
	me *types.Validator,
	quorumHash tmbytes.HexBytes,
) ([]*types.Validator, error) {
	// Build the deterministic list of quorum members:
	// 1. Retrieve the deterministic masternode list which is valid at quorumHeight
	// 2. Calculate SHA256(proTxHash, quorumHash) for each entry in the list
	// 3. Sort the resulting list by the calculated hashes
	sortedValidators := newSortedValidatorList(validatorSetMembers, quorumHash)

	// Loop through the list until the member finds itself in the list. The index at which it finds itself is called i.
	meSortable := newSortableValidator(*me, quorumHash)
	i := float64(sortedValidators.index(meSortable))
	if i < 0 {
		return []*types.Validator{}, fmt.Errorf("current node is not a member of provided validator set")
	}

	// Calculate indexes (i+2^k)%n where k is in the range 0..floor(log2(n-1))-1
	// and n is equal to the size of the list.
	n := float64(sortedValidators.Len())
	count := math.Floor(math.Log2(n-1.0)) - 1.0
	if int(count) <= 0 {

		return []*types.Validator{}, fmt.Errorf("not enough validators: got %d, need at least %d", int(n), 5)
	}
	ret := make([]*types.Validator, 0, int(count))
	for k := float64(0); k <= count; k++ {
		index := int(math.Mod(i+math.Pow(2, k), n))
		// Add addresses of masternodes at indexes calculated at previous step
		// to the set of deterministic connections.
		ret = append(ret, sortedValidators[index].Validator.Copy())
	}

	return ret, nil
}
