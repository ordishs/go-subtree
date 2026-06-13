# go-subtree Specification

Module: `github.com/bsv-blockchain/go-subtree` (package `subtree`)
Go version: 1.25+

This document specifies the data structures, algorithms, wire formats, and behavioural contracts implemented by the `subtree` package. It is derived directly from the Go source code and is intended to be sufficient for an independent, byte-compatible re-implementation.

---

## 1. Purpose and Scope

`go-subtree` manages **subtrees**: fixed-capacity merkle trees over Bitcoin SV transaction identifiers. In Teranode-style block assembly, a block's transaction set is partitioned into many subtrees of equal capacity (a power of two). Each subtree tracks, per leaf, the transaction id, its fee, and its serialized size. Subtree roots can then be composed into a single block merkle root, with proofs that are equivalent to those of one flat merkle tree over all transactions.

The package provides:

- The `Subtree` structure: leaf management, merkle root and merkle proof computation, binary serialization/deserialization.
- The `Data` structure: full transaction payloads (`*bt.Tx`) aligned index-for-index with a subtree's leaves.
- The `Meta` structure: per-leaf parent-transaction inpoint metadata (`TxInpoints`) aligned with a subtree's leaves.
- `TxInpoints`: a packed, deduplicated representation of a transaction's parent txids and the vout indexes consumed from each.
- Coinbase placeholder conventions for trees built before the coinbase transaction exists.
- Optional file-backed `mmap` storage for leaf arrays (Unix only).
- Power-of-two helpers and generic `Min`/`Max`.

External dependencies (direct):

| Module | Use |
|---|---|
| `github.com/bsv-blockchain/go-bt/v2` | `bt.Tx`, `bt.Input`, `chainhash.Hash` (32-byte double-SHA256 hashes) |
| `github.com/bsv-blockchain/go-safe-conversion` | checked int conversions |
| `github.com/bsv-blockchain/go-tx-map` | SwissMap implementation backing `TxMap` |

All multi-byte integers in every wire format in this package are **little-endian**. All hashes are 32 bytes (`chainhash.HashSize`).

---

## 2. Core Data Structures

### 2.1 Node

A leaf of a subtree.

```go
type Node struct {
    Hash        chainhash.Hash `json:"txid"` // transaction id
    Fee         uint64         `json:"fee"`
    SizeInBytes uint64         `json:"size"`
}
```

- JSON field name for `Hash` is deliberately `txid` so UIs can link to `/tx/<txid>`.
- `Node` contains no pointers; its in-memory size is exactly 48 bytes (32 + 8 + 8). This property is load-bearing for the mmap backend (§8).

### 2.2 Subtree

```go
type Subtree struct {
    Height           int
    Fees             uint64
    SizeInBytes      uint64
    FeeHash          chainhash.Hash    // reserved; currently always zero
    Nodes            []Node
    ConflictingNodes []chainhash.Hash  // txids needing conflict checks at block assembly

    // unexported:
    rootHash  *chainhash.Hash          // cached root; invalidated on mutation
    treeSize  int                      // leaf capacity = 2^Height
    mu        sync.RWMutex             // protects Nodes for the locked accessors
    nodeIndex map[chainhash.Hash]int   // lazy txid → leaf-index map
    closer    io.Closer                // non-nil iff mmap-backed
}
```

Invariants:

- `treeSize = 2^Height`; `len(Nodes) <= treeSize`.
- `Fees` = sum of `Nodes[i].Fee`; `SizeInBytes` = sum of `Nodes[i].SizeInBytes` (maintained incrementally by add/remove/replace operations; `ReplaceRootNode` only **adds** the new node's size, it does not subtract the replaced node's — see §4.4).
- `rootHash` is a cache; every mutation of `Nodes` resets it to `nil`.
- `ConflictingNodes` lists txids that are also present in `Nodes` and were flagged as conflicting (e.g. double-spend candidates) for later resolution during block assembly.

### 2.3 TxMap interface

```go
type TxMap interface {
    Put(hash chainhash.Hash, value uint64) error
    Get(hash chainhash.Hash) (uint64, bool)
    Exists(hash chainhash.Hash) bool
    Length() int
    Keys() []chainhash.Hash
}
```

`Subtree.GetMap()` returns a `TxMap` (a SwissMap from `go-tx-map`) mapping every leaf hash to its leaf index. `Subtree.Difference(ids TxMap)` returns, in leaf order, all nodes whose hash is absent from `ids`.

---

## 3. Construction

| Constructor | Behaviour |
|---|---|
| `NewTree(height int)` | Capacity `2^height`. Errors with `ErrHeightNegative` if `height < 0`. Height 0 is legal (capacity 1). |
| `NewTreeByLeafCount(n int)` | Requires `n` to be an exact power of two (`ErrNotPowerOfTwo` otherwise); height = `log2(n)`. |
| `NewIncompleteTreeByLeafCount(n int)` | Height = `ceil(log2(n))`; `n` need not be a power of two. |
| `NewTreeMmap(height, dir)` | As `NewTree` but `Nodes` is backed by a file-backed mmap region created in `dir` (§8). |
| `NewTreeByLeafCountMmap(n, dir)` | As `NewTreeByLeafCount`, mmap-backed. |
| `NewSubtreeFromBytes(b []byte)` | Deserializes the full wire format (§6.1). |
| `NewSubtreeFromReader(r io.Reader)` | Streaming deserialization of the full wire format. |
| `NewSubtreeFromReaderMmap(r, dir)` | Streaming deserialization with leaves written directly into an mmap region. |

`NewSubtreeFromBytes` / `NewSubtreeFromReader` install a `recover()` guard that logs and converts panics during deserialization into a logged recovery (the constructors return the underlying error from deserialization; the deserializers themselves convert panics to errors).

### Lifecycle

- `Close() error` — for mmap-backed subtrees, unmaps memory and deletes the backing file; no-op (returns `nil`) for heap-backed subtrees and nil receivers. Idempotent.
- `IsMmapBacked() bool` — true iff a closer is attached.
- `Duplicate() *Subtree` — deep copy of `Nodes` and `ConflictingNodes`; copies scalar fields and the cached root pointer. The copy is always heap-backed (no closer, no nodeIndex, fresh mutex).

---

## 4. Leaf Operations

### 4.1 Counters and predicates

- `Size() int` — leaf **capacity** (`cap(Nodes)`). Read-locked.
- `Length() int` — current leaf count (`len(Nodes)`). Read-locked.
- `IsComplete() bool` — `len(Nodes) == cap(Nodes)`. Read-locked.

### 4.2 Adding leaves

All add operations fail with `ErrSubtreeFull` when the tree already holds `treeSize` leaves, append the node, reset the cached root, add the node's fee and size to the running totals, and (if the lazy `nodeIndex` map already exists) record the new node's index.

| Method | Locking | Coinbase-placeholder guard |
|---|---|---|
| `AddNode(hash, fee, size)` | none — **single-goroutine only** | rejects `CoinbasePlaceholder` with `ErrCoinbasePlaceholderMisuse` |
| `AddSubtreeNode(node Node)` | full mutex | rejects placeholder |
| `AddSubtreeNodeWithoutLock(node Node)` | none | **does not** check for the placeholder |
| `AddCoinbaseNode()` | none | only valid on an **empty** subtree (`ErrSubtreeNotEmpty` otherwise); appends the placeholder node with fee 0 / size 0 and resets `Fees` and `SizeInBytes` to 0 |

### 4.3 Removing leaves

`RemoveNodeAtIndex(index int) error` (mutex-locked): bounds-checks (`ErrIndexOutOfRange` when `index >= len(Nodes)`), subtracts the node's fee and size from the totals, removes it by slice splice (shifting all later leaves left by one), resets the cached root, and deletes the hash from `nodeIndex` if present.

### 4.4 Replacing the root leaf

`ReplaceRootNode(hash, fee, sizeInBytes) *chainhash.Hash` overwrites `Nodes[0]` (or appends if the tree is empty), resets the cached root, **adds** `sizeInBytes` to `SizeInBytes` (it does not subtract the replaced leaf's size, and does not touch `Fees`), and returns the freshly computed root. Intended use: swapping the coinbase placeholder for the real coinbase txid once it is known.

`RootHashWithReplaceRootNode(hash, fee, size) (*chainhash.Hash, error)` performs the same substitution **on a duplicate** and returns the resulting root without mutating the receiver. Returns `ErrSubtreeNil` on a nil receiver.

### 4.5 Lookup

- `NodeIndex(hash) int` — returns the leaf index or `-1`. On first call it builds `nodeIndex` (a `map[chainhash.Hash]int` over all current leaves) under the mutex; subsequent adds/removes maintain it. The map read itself is unlocked.
- `HasNode(hash) bool` — `NodeIndex(hash) != -1`.
- `GetNode(hash) (*Node, error)` — pointer into `Nodes`, or `ErrNodeNotFound`.
- `GetMap() (TxMap, error)` — SwissMap of hash → index for all leaves.
- `Difference(ids TxMap) ([]Node, error)` — leaves not present in `ids` (never returns a non-nil error in the current implementation).

### 4.6 Conflicting nodes

`AddConflictingNode(hash) error`:

1. Verifies the hash is one of the subtree's leaves; otherwise returns `ErrConflictingNodeNotInSubtree`.
2. Deduplicates: if already recorded, returns `nil` silently.
3. Appends to `ConflictingNodes`.

---

## 5. Merkle Computation

### 5.1 Hash rule

Parent hash = double SHA-256 of the 64-byte concatenation `left ‖ right` (standard Bitcoin merkle rule). Two special cases, applied **at every level**, implemented in `calcMerkle`:

- If the left child is the zero hash, the parent is the zero hash (empty branch propagation).
- If the right child is the zero hash (odd count at that level), the parent is `dsha256(left ‖ left)` — Bitcoin's duplicate-last-when-odd rule.

### 5.2 BuildMerkleTreeStoreFromBytes

`BuildMerkleTreeStoreFromBytes(nodes []Node) (*[]chainhash.Hash, error)` computes all internal levels of the merkle tree over the leaf hashes:

- Empty input → pointer to an empty slice (no error).
- Single leaf → the store is `[leafHash]` (Bitcoin convention: the merkle root of one transaction is that transaction's hash).
- Otherwise: let `nextPoT = NextPowerOfTwo(len(nodes))`. The store has `nextPoT − 1` entries and holds the internal levels bottom-up: entries `[0, nextPoT/2)` are the parents of the (zero-padded) leaf level, the following `nextPoT/4` entries are the next level, and so on; **the final entry of the store is the merkle root**. Leaves themselves are not stored. Positions whose subtree contains no real leaves hold the zero hash.
- Levels whose width exceeds 1024 node-pairs are computed in parallel: the range is split into chunks of `routineSplitSize = 1024` pairs, one goroutine per chunk, joined with a `WaitGroup` before the next level starts.
- Reading "leaf" inputs beyond `len(nodes)` yields the zero hash (`getNodeHashAt`), which feeds the §5.1 special cases.

### 5.3 RootHash

`RootHash() *chainhash.Hash`:

- nil receiver → `nil`; empty subtree → `nil`.
- Returns the cached root if present; otherwise builds the full store (§5.2), caches the last entry as the root, and returns it. Internal build errors yield `nil` (not an error).

### 5.4 RootHashPadded

`RootHashPadded(targetHeight int) (*chainhash.Hash, error)` lifts the subtree's natural root to `targetHeight` by repeated self-hashing — `root = dsha256(root ‖ root)` once per missing level — so that compositions of subtree roots reproduce the root of one flat merkle tree over all leaves (phantom-slot version of the duplicate-when-odd rule).

- nil receiver → `ErrSubtreeNil`.
- Empty subtree → `(nil, nil)` (mirrors `RootHash`'s nil-for-empty contract).
- The subtree's leaf count need **not** be a power of two. The natural height is computed as `bits.Len(uint(length − 1))`, i.e. `ceil(log2(length))` with the height-0 case for `length == 1`.
- `targetHeight < actualHeight` → `ErrTargetHeightTooSmall`.

### 5.5 Merkle proofs

`GetMerkleProof(index int) ([]*chainhash.Hash, error)` returns the sibling-hash path for the leaf at `index`, ordered leaf-level first, root-adjacent last (`ceil(log2(len(Nodes)))` entries).

- `index >= len(Nodes)` → `ErrIndexOutOfRange`.
- Leaf-level sibling: for even `index` the sibling is `index+1`, except when `index+1` is past the last leaf, in which case the leaf's own hash is used (duplicate-when-odd). For odd `index` the sibling is `index−1`.
- Higher levels are read from the §5.2 store. When the natural sibling position holds the zero hash (phantom branch), the node's own position is used instead, i.e. the node stands in for its missing sibling.
- The current implementation builds the entire merkle store for each proof (noted in-source as a candidate for optimization).

### 5.6 Coinbase proof across subtrees

`GetMerkleProofForCoinbase(subtrees []*Subtree) ([]*chainhash.Hash, error)`:

1. Errors with `ErrNoSubtreesAvailable` on an empty slice.
2. Takes the merkle proof of leaf 0 within `subtrees[0]` (the coinbase position).
3. Builds a **top tree** with capacity `CeilPowerOfTwo(len(subtrees))` whose leaves are each subtree's root hash (with that subtree's `Fees` and `SizeInBytes`).
4. Appends the top tree's proof for leaf 0.

The concatenated path proves the coinbase up to the block merkle root.

---

## 6. Subtree Wire Format

### 6.1 Full serialization — `Serialize()` / `Deserialize` / `DeserializeFromReader`

All integers little-endian; hashes raw 32 bytes.

| Offset | Size | Field |
|---|---|---|
| 0 | 32 | Root hash (written for integrity checking; recomputed `RootHash()` at serialize time) |
| 32 | 8 | `Fees` (uint64) |
| 40 | 8 | `SizeInBytes` (uint64) |
| 48 | 8 | `numLeaves` = `len(Nodes)` (uint64) |
| 56 | 48 × numLeaves | Leaf records: `hash` (32) ‖ `fee` (8, uint64) ‖ `sizeInBytes` (8, uint64) |
| … | 8 | `numConflicting` (uint64) |
| … | 32 × numConflicting | Conflicting txids (32 bytes each) |

Deserialization notes:

- The stored root hash is read into the root cache **as-is**; it is not re-verified against the leaves.
- `Height` is reconstructed as `ceil(log2(numLeaves))` and `treeSize` is set to `numLeaves` (not to `2^Height`), so a deserialized subtree is exactly full at its serialized leaf count: further `AddNode` calls on a deserialized subtree fail with `ErrSubtreeFull` once `numLeaves` is reached.
- Reads go through a 32 KiB `bufio.Reader`; every field uses `io.ReadFull` and failures are wrapped errors.
- `Deserialize([]byte)` and the reader-based deserializers convert panics into errors via `recover`.

### 6.2 Pooled-buffer deserialization — `DeserializeFromReaderWithAllocator`

```go
type NodeAllocator func(numLeaves int) []Node
```

Identical wire semantics to §6.1, but the backing array for `Nodes` is obtained from a caller-supplied allocator (e.g. a `sync.Pool`), enabling reuse of per-subtree leaf arrays across blocks. Contract:

- The allocator must return a slice with `cap >= numLeaves`; the deserializer reslices it to `[:numLeaves]`. If the returned cap is too small (or `alloc` is nil) the deserializer transparently falls back to `make([]Node, numLeaves)`.
- Ownership of the backing array transfers to the `Subtree` until `ReleaseNodes()` is called.
- `ReleaseNodes() []Node` returns the **full backing array** (resliced to its cap, so a pool reuses the whole capacity) and sets `st.Nodes = nil`. Returns nil if there are no nodes (already released, or mmap-backed). After release the subtree must not be used to read or mutate leaf data.

`DeserializeFromReader(r)` is exactly `DeserializeFromReaderWithAllocator(r, nil)`.

### 6.3 Nodes-only serialization — `SerializeNodes()` / `DeserializeNodesFromReader`

- `SerializeNodes()` writes only the concatenated 32-byte leaf hashes (`32 × len(Nodes)` bytes) — no header, fees, sizes, or conflicts.
- `DeserializeNodesFromReader(r)` is **not** the inverse of `SerializeNodes`. It parses the §6.1 full format, skipping the root/fees/size header, and returns the leaf **hashes only** as one `[]byte` of `32 × numLeaves` bytes (each leaf's 16 bytes of fee+size are read and discarded).

### 6.4 Conflicts-only extraction — `DeserializeSubtreeConflictingFromReader`

Parses the §6.1 format but skips the header (48 bytes discarded) and all leaf records (`48 × numLeaves` discarded), returning only the `[]chainhash.Hash` of conflicting txids.

---

## 7. Companion Structures

### 7.1 Data — full transactions for a subtree

```go
type Data struct {
    Subtree *Subtree
    Txs     []*bt.Tx   // index-aligned with Subtree.Nodes
}
```

Constructors: `NewSubtreeData(subtree)` (empty `Txs` sized to `subtree.Size()`); `NewSubtreeDataFromBytes(subtree, bytes)` and `NewSubtreeDataFromReader(subtree, reader)` parse the wire format below against the supplied subtree.

**Wire format**: the raw Bitcoin serializations of the transactions, concatenated in leaf order, with **no framing** — transaction boundaries are recovered by `bt.Tx.ReadFrom`. If leaf 0 is the coinbase placeholder, position 0 is skipped on write; on read, a transaction that reports `IsCoinbase()` while the placeholder occupies leaf 0 is stored at index 0 without hash validation.

Validation on read (`serializeFromReader`, used by both constructors):

- Requires a non-nil subtree with at least one leaf (`ErrSubtreeNodesEmpty`).
- Every non-coinbase transaction's `TxIDChainHash()` must equal the leaf hash at its position (`ErrTxHashMismatch`).
- More transactions than leaves → `ErrTxIndexOutOfBounds`. EOF terminates the stream normally (fewer transactions than leaves is not an error at read time).

API:

- `RootHash()` — delegates to the subtree.
- `AddTx(tx, index)` — stores `tx` at `index` after verifying its txid against the leaf hash; the coinbase is accepted unverified at index 0 when leaf 0 is the placeholder.
- `Serialize()` — concatenated transactions, starting after the placeholder if present. Every required slot must be non-nil (`ErrSubtreeLengthMismatch` if a non-coinbase slot is empty); requires `Subtree != nil` (`ErrCannotSerializeSubtreeNotSet`).
- Streaming helpers for chunked I/O over large subtrees:
  - `WriteTransactionsToWriter(w, startIdx, endIdx)` — streams `Txs[startIdx:endIdx)` with `tx.SerializeTo`, skipping the placeholder at 0; a nil entry in range is `ErrTransactionNil`.
  - `ReadTransactionsFromReader(r, startIdx, endIdx)` — reads transactions into `Txs[startIdx:endIdx)` with per-position hash validation; returns the count read; EOF ends early without error.
  - Package-level `WriteTransactionChunk(w, txs)` — writes a plain slice, silently skipping nils.
  - Package-level `ReadTransactionChunk(r, subtree, startIdx, count)` — reads up to `count` transactions, validating each against the subtree leaf at `startIdx+i`; returns the populated slice.

### 7.2 Meta — per-leaf inpoint metadata

```go
type Meta struct {
    Subtree    *Subtree
    TxInpoints []TxInpoints   // index-aligned with Subtree.Nodes
    rootHash   chainhash.Hash // unexported; set on serialize / read on deserialize
}
```

Constructors: `NewSubtreeMeta(subtree)` (empty, sized to `subtree.Size()`); `NewSubtreeMetaFromBytes` / `NewSubtreeMetaFromReader` (parse the wire format below).

**Wire format**:

| Size | Field |
|---|---|
| 32 | Subtree root hash |
| 4 | `count` (uint32) — number of TxInpoints records |
| … | `count` consecutive `TxInpoints` serializations (§7.3.3) |

The stored root hash is read but not verified against the supplied subtree.

API:

- `SetTxInpointsFromTx(tx)` — locates the tx's leaf by `NodeIndex` (`ErrNodeNotFound` if absent) and stores `NewTxInpointsFromTx(tx)` at that index.
- `SetTxInpoints(idx, txInpoints)` — bounds-checked direct store (`ErrIndexOutOfRange`).
- `GetParentTxHashes(index)` / `GetTxInpoints(index)` — bounds-checked accessors delegating to the `TxInpoints` at that leaf.
- `Serialize()` — requires `Subtree != nil`; for every leaf `i > 0`, `TxInpoints[i].ParentTxHashes` must be non-nil (set), otherwise serialization fails. Index 0 (coinbase position) is exempt. Writes `count = Subtree.Length()` records.

The nil-versus-empty distinction is contractual: `nil` `ParentTxHashes` means "not set yet" (serialization error); an **empty** non-nil slice means "this transaction has no parents" and serializes legally (see `NewTxInpoints`).

### 7.3 TxInpoints

#### 7.3.1 Model

For one transaction, `TxInpoints` records the **deduplicated** parent txids referenced by its inputs and, per parent, the vout indexes consumed:

```go
type Inpoint struct {
    Hash  chainhash.Hash
    Index uint32
}

type TxInpoints struct {
    ParentTxHashes []chainhash.Hash // deduplicated, in first-seen order
    voutIdxs       []uint32         // packed: [c_0, v_0_0..v_0_{c_0-1}, c_1, v_1_0..., ...]
    SubtreeIndex   int16            // runtime-only; 0 = unassigned, otherwise chainedSubtrees index + 1; never serialized
}
```

`voutIdxs` is a single packed allocation: for each parent (in `ParentTxHashes` order) one count word `c_i ≥ 1` followed by `c_i` vout values. Invariants: if `len(ParentTxHashes) == 0` then `voutIdxs` is empty; otherwise `len(voutIdxs) ≥ len(ParentTxHashes)` and total vout values = `len(voutIdxs) − len(ParentTxHashes)`. The packed layout exactly matches the wire format, so deserialization fills one allocation directly. (An earlier public field `Idxs [][]uint32` was removed; access goes through `GetParentVoutsAtIndex` / `GetTxInpoints`.)

#### 7.3.2 Constructors and accessors

- `NewTxInpoints()` — empty placeholder with **non-nil but empty** `ParentTxHashes` (see §7.2 nil/empty contract); allocation-free.
- `NewTxInpointsFromTx(tx)` / `NewTxInpointsFromInputs(inputs)` — build from inputs with buffers pre-sized to worst case (`cap = n` parents, `cap = 2n` packed words for `n` inputs), so construction never reallocates. Deduplication preserves first-seen parent order; a repeated parent's new vout is inserted at the end of that parent's run (count incremented, tail shifted).
- `NewTxInpointsFromPacked(parents, voutIdxs)` — zero-copy, zero-validation aliasing constructor for trusted hot paths. Caller guarantees layout correctness and that the supplied slices outlive all uses; malformed input manifests as garbage reads or panics at access time.
- `NewTxInpointsFromBytes(data)` / `NewTxInpointsFromReader(r)` — defensive construction from the wire format.
- `GetParentTxHashes() []chainhash.Hash`; `GetParentTxHashAtIndex(i)` (bounds-checked, `ErrIndexOutOfRange`).
- `GetParentVoutsAtIndex(i) ([]uint32, error)` — bounds-checked; the returned slice **aliases internal storage** and must be treated as read-only. Locating parent `i` walks the packed runs: O(P) in parent count.
- `GetTxInpoints() []Inpoint` — flattens to (hash, vout) pairs in parent-then-vout order; freshly allocated, caller-owned.
- `String()` — renders in the legacy `TxInpoints{ParentTxHashes: …, Idxs: …}` shape for log/test compatibility.

#### 7.3.3 TxInpoints wire format

| Size | Field |
|---|---|
| 4 | `P` = number of parents (uint32) |
| 32 × P | Parent txids |
| per parent, in order | `c_i` (uint32) followed by `c_i` × vout (uint32 each) |

`Serialize()` validates the packed-layout invariant first (`ErrParentTxHashesMismatch` on violation). A `P` of 0 is legal and is followed by nothing. List lengths are clamped via `len32` (lengths above `MaxUint32` are clamped rather than erroring — practically unreachable).

---

## 8. Mmap-Backed Leaf Storage

Purpose: keep very large leaf arrays out of the Go heap (no GC scanning, OS may page cold data to disk). Available on Unix-like systems only; on Windows `mmapFile` always errors, so the mmap constructors fail gracefully.

Mechanism (`newFileBackedMmapNodes(capacity, dir)`):

1. `capacity <= 0` → `ErrCapacityNotPositive`.
2. Create a temp file `subtree-nodes-*` in `dir`; truncate to `capacity × 48` bytes.
3. `mmap` it `PROT_READ|PROT_WRITE`, `MAP_SHARED` (writes flush to the file, letting the OS reclaim RAM).
4. **Close the file descriptor immediately** — the kernel keeps the mapping alive through the inode, so no persistent fds are held (matters at thousands of concurrent subtrees).
5. Reinterpret the region as `[]Node` via `unsafe.Slice`, returned with `len = 0, cap = capacity`. Safe precisely because `Node` is pointer-free (§2.1).

Cleanup: the returned `io.Closer` (an `mmapNodeStore`) munmaps and deletes the backing file; guarded by `sync.Once`, so `Close` is idempotent. `Subtree.Close()` delegates to it.

Constraints implied by the design:

- An mmap-backed subtree must not outlive its `Close()`.
- `ReleaseNodes()` deliberately returns nil for mmap-backed subtrees (their storage is not poolable).
- `deserializeFromReaderMmap` sizes the region to exactly `numLeaves` and on any subsequent read error closes the store (unmap + file removal) before returning.

---

## 9. Coinbase Placeholder Conventions

Block assembly starts before the coinbase transaction exists. The package reserves a sentinel txid:

- `CoinbasePlaceholder` — `[32]byte` of all `0xFF`; `CoinbasePlaceholderHashValue` / `CoinbasePlaceholderHash` are its `chainhash.Hash` value/pointer forms.
- The placeholder may only enter a tree via `AddCoinbaseNode()` on an **empty** subtree (leaf 0, fee 0, size 0). `AddNode` and `AddSubtreeNode` actively reject it (`ErrCoinbasePlaceholderMisuse`); `AddSubtreeNodeWithoutLock` does not check.
- `Data` and `Meta` treat leaf 0 specially when it holds the placeholder: `Data` skips it during serialization and accepts the real coinbase unvalidated at index 0; `Meta.Serialize` exempts index 0 from the "inpoints must be set" rule.
- `ReplaceRootNode` / `RootHashWithReplaceRootNode` are the intended mechanism for substituting the real coinbase txid at leaf 0.
- `IsCoinbasePlaceHolderTx(tx)` identifies the canonical placeholder **transaction**: an input-less, output-less `bt.Tx` with `Version = 0xFFFFFFFF` and `LockTime = 0xFFFFFFFF` (compared by txid).
- `FrozenBytes` (36 × `0xFF`), `FrozenBytesTxBytes` (first 32), `FrozenBytesTxHash` — sentinel constants for frozen/placeholder transaction outpoints.

---

## 10. Utility Functions

| Function | Contract |
|---|---|
| `CeilPowerOfTwo(n int) int` | Smallest power of two ≥ `n`; returns 1 for `n ≤ 0`. (Float-log based.) |
| `IsPowerOfTwo(n int) bool` | `n > 0 && n & (n−1) == 0`. |
| `NextPowerOfTwo(n int) int` | `n` if already a power of two (note: this branch also returns 0 for 0), else the next higher power of two. |
| `NextLowerPowerOfTwo(x uint) uint` | Highest power of two ≤ `x`; 0 for 0. (Bit-length based.) |
| `Min[T cmp.Ordered](a, b T) T` / `Max[T cmp.Ordered](a, b T) T` | Generic two-value min/max. |

---

## 11. Concurrency Contract

The `Subtree` mutex protects only the methods that take it; the API is intentionally split between locked and unlocked variants:

- **Locked (safe under concurrent use):** `Size`, `Length`, `IsComplete` (read lock); `AddSubtreeNode`, `RemoveNodeAtIndex` (write lock); `NodeIndex` takes the write lock only while building the lazy index map.
- **Unlocked (single-goroutine by contract):** `AddNode` (explicitly documented as not concurrency-safe), `AddSubtreeNodeWithoutLock`, `AddCoinbaseNode`, `ReplaceRootNode`, `AddConflictingNode`, all serialization/deserialization, `RootHash` and proof generation, and the map read inside `NodeIndex`/`HasNode`/`GetNode`.

Mixing the locked mutators with the unlocked readers/mutators concurrently is not safe. The intended pattern is single-writer construction (hot path uses `AddNode`), with the locked accessors available for observers.

`BuildMerkleTreeStoreFromBytes` parallelizes internally (§5.2) but takes an immutable snapshot of the leaf slice; callers must not mutate leaves during root/proof computation.

`TxInpoints`, `Data`, and `Meta` have no internal locking.

---

## 12. Error Catalogue

All errors are package-level sentinels (`errors.New`), match-able with `errors.Is`; functions wrap them with context via `fmt.Errorf("…: %w", …)`.

| Sentinel | Meaning |
|---|---|
| `ErrIndexOutOfRange` | Index outside valid range (leaves, inpoints, meta) |
| `ErrTxIndexOutOfBounds` | More transactions in a Data stream than subtree leaves |
| `ErrHeightNegative` | `NewTree` with height < 0 |
| `ErrNotPowerOfTwo` | Leaf count not a power of two where required |
| `ErrSubtreeFull` | Add on a subtree at capacity |
| `ErrSubtreeNil` | Nil subtree receiver where disallowed |
| `ErrSubtreeNotEmpty` | `AddCoinbaseNode` on a non-empty subtree |
| `ErrSubtreeNodesEmpty` | Data operations on a subtree with no leaves |
| `ErrNoSubtreesAvailable` | `GetMerkleProofForCoinbase` with no subtrees |
| `ErrCoinbasePlaceholderMisuse` | Placeholder added via the wrong method |
| `ErrConflictingNodeNotInSubtree` | Conflicting txid not among the leaves |
| `ErrNodeNotFound` | Hash lookup miss |
| `ErrTargetHeightTooSmall` | `RootHashPadded` target below natural height |
| `ErrParentTxHashesMismatch` | `TxInpoints` packed-layout invariant violated at serialize |
| `ErrTxHashMismatch` | Transaction hash ≠ leaf hash at the same index |
| `ErrSubtreeLengthMismatch` | `Data.Serialize` with missing (nil) transactions |
| `ErrCannotSerializeSubtreeNotSet` | `Data`/`Meta` serialize without a subtree (also wraps unset inpoints) |
| `ErrReadError` | Generic read error (testing aid) |
| `ErrTransactionNil` | Nil transaction in a required write slot |
| `ErrTransactionWrite` / `ErrTransactionRead` | Wrapped stream I/O failures |
| `ErrCapacityNotPositive` | Mmap allocation with capacity ≤ 0 |

---

## 13. Performance Characteristics

Documented/benchmarked properties the implementation commits to:

- `AddNode` is allocation-free (~12 ns/op on Apple M1 Max) — no locking, incremental totals, packed `Node` value.
- `Serialize` / `SerializeNodes` are allocation-light (single pre-sized buffer).
- Deserialization streams through a 32 KiB buffered reader; the allocator variant (§6.2) exists to eliminate per-block leaf-array churn (cited in-source: ~29 GB per 654M-tx block on the validator hot path).
- Merkle building parallelizes levels wider than 1024 pairs across goroutines.
- `TxInpoints`' packed layout halves heap-object count for typical 1–2-input transactions versus the previous nested-slice layout; construction from inputs never reallocates.
- Root hashes are cached and invalidated on mutation; `NodeIndex` lookups are O(1) after the lazy map is built.
