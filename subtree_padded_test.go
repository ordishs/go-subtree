package subtree

import (
	"crypto/sha256"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/stretchr/testify/require"
)

func TestRootHashPadded_LiftsTwoLeafSubtreeToHeightEight(t *testing.T) {
	st, err := NewTreeByLeafCount(256)
	require.NoError(t, err)

	h1, _ := chainhash.NewHashFromStr("97af9ad3583e2f83fc1e44e475e3a3ee31ec032449cc88b491479ef7d187c115")
	h2, _ := chainhash.NewHashFromStr("7ce05dda56bc523048186c0f0474eb21c92fe35de6d014bd016834637a3ed08d")

	require.NoError(t, st.AddNode(*h1, 0, 0))
	require.NoError(t, st.AddNode(*h2, 0, 0))

	lifted, err := st.RootHashPadded(8)
	require.NoError(t, err)
	require.NotNil(t, lifted)

	expected := *st.RootHash()
	for range 7 {
		var buf [64]byte
		copy(buf[0:32], expected[:])
		copy(buf[32:64], expected[:])
		first := sha256.Sum256(buf[:])
		expected = chainhash.Hash(sha256.Sum256(first[:]))
	}

	require.Equal(t, expected.String(), lifted.String())
}
