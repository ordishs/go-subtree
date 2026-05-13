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

func TestRootHashPadded_NoLiftWhenAlreadyAtTargetHeight(t *testing.T) {
	st, err := NewTreeByLeafCount(4)
	require.NoError(t, err)

	for i := range 4 {
		h := chainhash.HashH([]byte{byte(i)})
		require.NoError(t, st.AddNode(h, 0, 0))
	}

	padded, err := st.RootHashPadded(2)
	require.NoError(t, err)
	require.Equal(t, st.RootHash().String(), padded.String())
}

func TestRootHashPadded_EmptySubtreeReturnsNil(t *testing.T) {
	st, err := NewTreeByLeafCount(4)
	require.NoError(t, err)

	padded, err := st.RootHashPadded(2)
	require.NoError(t, err)
	require.Nil(t, padded)
}

func TestRootHashPadded_NotPowerOfTwoErrors(t *testing.T) {
	st, err := NewTreeByLeafCount(4)
	require.NoError(t, err)

	h := chainhash.HashH([]byte{0x01})
	require.NoError(t, st.AddNode(h, 0, 0))
	h2 := chainhash.HashH([]byte{0x02})
	require.NoError(t, st.AddNode(h2, 0, 0))
	h3 := chainhash.HashH([]byte{0x03})
	require.NoError(t, st.AddNode(h3, 0, 0))

	_, err = st.RootHashPadded(2)
	require.ErrorIs(t, err, ErrNotPowerOfTwoLeafCount)
}

func TestRootHashPadded_TargetHeightTooSmallErrors(t *testing.T) {
	st, err := NewTreeByLeafCount(4)
	require.NoError(t, err)

	for i := range 4 {
		h := chainhash.HashH([]byte{byte(i)})
		require.NoError(t, st.AddNode(h, 0, 0))
	}

	_, err = st.RootHashPadded(1)
	require.ErrorIs(t, err, ErrTargetHeightTooSmall)
}

func TestRootHashPadded_MatchesBigTreeRoot(t *testing.T) {
	bigTree, err := NewTreeByLeafCount(16)
	require.NoError(t, err)

	hashes := make([]chainhash.Hash, 10)
	for i := range hashes {
		hashes[i] = chainhash.HashH([]byte{byte(i + 1)})
		require.NoError(t, bigTree.AddNode(hashes[i], 0, 0))
	}

	left, err := NewTreeByLeafCount(8)
	require.NoError(t, err)
	for i := range 8 {
		require.NoError(t, left.AddNode(hashes[i], 0, 0))
	}

	right, err := NewTreeByLeafCount(8)
	require.NoError(t, err)
	for i := 8; i < 10; i++ {
		require.NoError(t, right.AddNode(hashes[i], 0, 0))
	}

	leftRoot, err := left.RootHashPadded(3)
	require.NoError(t, err)
	rightRoot, err := right.RootHashPadded(3)
	require.NoError(t, err)

	top, err := NewTreeByLeafCount(2)
	require.NoError(t, err)
	require.NoError(t, top.AddNode(*leftRoot, 0, 0))
	require.NoError(t, top.AddNode(*rightRoot, 0, 0))

	require.Equal(t, bigTree.RootHash().String(), top.RootHash().String())
}
