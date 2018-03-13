package chain

import (
	"errors"
	"sync"

	"github.com/vechain/thor/block"
	"github.com/vechain/thor/cache"
	"github.com/vechain/thor/chain/persist"
	"github.com/vechain/thor/kv"
	"github.com/vechain/thor/thor"
	"github.com/vechain/thor/tx"
)

const (
	headerCacheLimit   = 512
	bodyCacheLimit     = 512
	blockTxIDsLimit    = 1024
	receiptsCacheLimit = 512
)

var errNotFound = errors.New("not found")

// Chain describes a persistent block chain.
// It's thread-safe.
type Chain struct {
	kv        kv.GetPutter
	bestBlock *block.Block
	cached    cached
	rw        sync.RWMutex
}

type cached struct {
	header     *cache.LRU
	body       *cache.LRU
	blockTxIDs *cache.LRU
	receipts   *cache.LRU
}

// New create an instance of Chain.
func New(kv kv.GetPutter) *Chain {
	return &Chain{
		kv: kv,
		cached: cached{
			cache.NewLRU(headerCacheLimit),
			cache.NewLRU(bodyCacheLimit),
			cache.NewLRU(blockTxIDsLimit),
			cache.NewLRU(receiptsCacheLimit),
		},
	}
}

// WriteGenesis writes in genesis block.
// It will compare the given genesis block with the existed one. If not the same, an error returned.
func (c *Chain) WriteGenesis(genesis *block.Block) error {
	c.rw.Lock()
	defer c.rw.Unlock()

	b, err := c.getBlockByNumber(0)
	if err != nil {
		if !c.IsNotFound(err) {
			// errors occurred
			return err
		}
		// no genesis yet
		batch := c.kv.NewBatch()

		if err := persist.SaveBlock(batch, genesis); err != nil {
			return err
		}
		if err := persist.SaveTxLocations(batch, genesis.Transactions(), genesis.Header().ID()); err != nil {
			return err
		}
		if err := persist.SaveTrunkBlockID(batch, genesis.Header().ID()); err != nil {
			return err
		}
		if err := persist.SaveBestBlockID(batch, genesis.Header().ID()); err != nil {
			return err
		}
		if err := batch.Write(); err != nil {
			return err
		}
		c.bestBlock = genesis
		return nil
	}
	if b.Header().ID() != genesis.Header().ID() {
		return errors.New("genesis mismatch")
	}
	return nil
}

// AddBlock add a new block into block chain.
// The method will return nil immediately if the block already in the chain.
// Once reorg occurred, diff transactions are returned.
func (c *Chain) AddBlock(newBlock *block.Block, isTrunk bool) (tx.Transactions, error) {
	c.rw.Lock()
	defer c.rw.Unlock()

	if _, err := c.getBlock(newBlock.Header().ID()); err != nil {
		if !c.IsNotFound(err) {
			return nil, err
		}
	} else {
		// block already there
		return nil, nil
	}

	if _, err := c.getBlock(newBlock.Header().ParentID()); err != nil {
		if c.IsNotFound(err) {
			return nil, errors.New("parent missing")
		}
		return nil, err
	}

	batch := c.kv.NewBatch()
	if err := persist.SaveBlock(batch, newBlock); err != nil {
		return nil, err
	}

	diffTxsMap := make(map[thor.Hash]*tx.Transaction)
	if isTrunk {
		best, err := c.getBestBlock()
		if err != nil {
			return nil, err
		}

		_, oldBlocks, newBlocks, err := c.traceBackToCommonAncestor(best, newBlock)
		if err != nil {
			return nil, err
		}
		for _, ob := range oldBlocks {
			txs := ob.Transactions()
			if err := persist.EraseTrunkBlockID(batch, ob.Header().ID()); err != nil {
				return nil, err
			}
			if err := persist.EraseTxLocations(batch, txs); err != nil {
				return nil, err
			}
			for _, tx := range txs {
				diffTxsMap[tx.ID()] = tx
			}
		}

		for _, nb := range newBlocks {
			txs := nb.Transactions()
			if err := persist.SaveTrunkBlockID(batch, nb.Header().ID()); err != nil {
				return nil, err
			}
			if err := persist.SaveTxLocations(batch, txs, nb.Header().ID()); err != nil {
				return nil, err
			}
			for _, tx := range txs {
				delete(diffTxsMap, tx.ID())
			}
		}
		persist.SaveBestBlockID(batch, newBlock.Header().ID())
	}

	if err := batch.Write(); err != nil {
		return nil, err
	}

	c.cached.header.Add(newBlock.Header().ID(), newBlock.Header())
	c.cached.body.Add(newBlock.Header().ID(), newBlock.Body())

	if isTrunk {
		c.bestBlock = newBlock
	}

	var diffTxs tx.Transactions
	if len(diffTxsMap) > 0 {
		diffTxs = make(tx.Transactions, 0, len(diffTxsMap))
		for _, tx := range diffTxsMap {
			diffTxs = append(diffTxs, tx)
		}
	}
	return diffTxs, nil
}

// Think about the example below:
//
//   B1--B2--B3--B4--B5--B6
//             \
//              \
//               b4--b5
//
// When call traceBackToCommonAncestor(B6, b5), the return values will be:
// ([B5, B6, B4], [b5, b4], B3, nil)
func (c *Chain) traceBackToCommonAncestor(b1 *block.Block, b2 *block.Block) (*block.Block, []*block.Block, []*block.Block, error) {
	var (
		c1, c2 []*block.Block
		err    error
	)

	for {
		if b1.Header().Number() > b2.Header().Number() {
			c1 = append(c1, b1)
			if b1, err = c.getBlock(b1.Header().ParentID()); err != nil {
				return nil, nil, nil, err
			}
			continue
		}
		if b1.Header().Number() < b2.Header().Number() {
			c2 = append(c2, b2)
			if b2, err = c.getBlock(b2.Header().ParentID()); err != nil {
				return nil, nil, nil, err
			}
			continue
		}
		if b1.Header().ID() == b2.Header().ID() {
			return b1, c1, c2, nil
		}

		c1 = append(c1, b1)
		c2 = append(c2, b2)

		if b1, err = c.getBlock(b1.Header().ParentID()); err != nil {
			return nil, nil, nil, err
		}

		if b2, err = c.getBlock(b2.Header().ParentID()); err != nil {
			return nil, nil, nil, err
		}
	}
}

// GetBlockHeader get block header by block id.
func (c *Chain) GetBlockHeader(id thor.Hash) (*block.Header, error) {
	c.rw.RLock()
	defer c.rw.RUnlock()
	return c.getBlockHeader(id)
}

func (c *Chain) getBlockHeader(id thor.Hash) (*block.Header, error) {
	header, err := c.cached.header.GetOrLoad(id, func(interface{}) (interface{}, error) {
		return persist.LoadBlockHeader(c.kv, id)
	})
	if err != nil {
		return nil, err
	}
	return header.(*block.Header), nil
}

// GetBlockBody get block body by block id.
func (c *Chain) GetBlockBody(id thor.Hash) (*block.Body, error) {
	c.rw.RLock()
	defer c.rw.RUnlock()
	return c.getBlockBody(id)
}

func (c *Chain) getBlockBody(id thor.Hash) (*block.Body, error) {
	body, err := c.cached.body.GetOrLoad(id, func(interface{}) (interface{}, error) {
		return persist.LoadBlockBody(c.kv, id)
	})
	if err != nil {
		return nil, err
	}
	return body.(*block.Body), nil
}

// GetBlock get block by id.
func (c *Chain) GetBlock(id thor.Hash) (*block.Block, error) {
	c.rw.RLock()
	defer c.rw.RUnlock()

	return c.getBlock(id)
}

func (c *Chain) getBlock(id thor.Hash) (*block.Block, error) {
	header, err := c.getBlockHeader(id)
	if err != nil {
		return nil, err
	}
	body, err := c.getBlockBody(id)
	if err != nil {
		return nil, err
	}
	return block.Compose(header, body.Txs), nil
}

// GetBlockIDByNumber returns block id by block number on trunk.
func (c *Chain) GetBlockIDByNumber(num uint32) (thor.Hash, error) {
	c.rw.RLock()
	defer c.rw.RUnlock()
	return c.getBlockIDByNumber(num)
}

func (c *Chain) getBlockIDByNumber(num uint32) (thor.Hash, error) {
	id, err := persist.LoadTrunkBlockID(c.kv, num)
	if err != nil {
		return thor.Hash{}, err
	}
	return id, nil
}

// GetBlockByNumber get block on trunk by its number.
func (c *Chain) GetBlockByNumber(num uint32) (*block.Block, error) {
	c.rw.RLock()
	defer c.rw.RUnlock()
	return c.getBlockByNumber(num)
}

func (c *Chain) getBlockByNumber(num uint32) (*block.Block, error) {
	id, err := c.getBlockIDByNumber(num)
	if err != nil {
		return nil, err
	}
	return c.getBlock(id)
}

// GetBestBlock get the newest block on trunk.
func (c *Chain) GetBestBlock() (*block.Block, error) {
	c.rw.RLock()
	defer c.rw.RUnlock()

	return c.getBestBlock()
}

func (c *Chain) getBestBlock() (*block.Block, error) {

	if best := c.bestBlock; best != nil {
		return best, nil
	}
	id, err := persist.LoadBestBlockID(c.kv)
	if err != nil {
		return nil, err
	}
	best, err := c.getBlock(id)
	if err != nil {
		return nil, err
	}
	c.bestBlock = best
	return best, nil
}

// GetTransaction get transaction by id on trunk.
func (c *Chain) GetTransaction(txID thor.Hash) (*tx.Transaction, *persist.TxLocation, error) {
	c.rw.RLock()
	defer c.rw.RUnlock()

	return c.getTransaction(txID)
}

func (c *Chain) getTransaction(txID thor.Hash) (*tx.Transaction, *persist.TxLocation, error) {
	loc, err := persist.LoadTxLocation(c.kv, txID)
	if err != nil {
		return nil, nil, err
	}
	body, err := c.getBlockBody(loc.BlockID)
	if err != nil {
		return nil, nil, err
	}
	return body.Txs[loc.Index], loc, nil
}

func (c *Chain) getTransactionIDs(blockID thor.Hash) (map[thor.Hash]int, error) {
	ids, err := c.cached.blockTxIDs.GetOrLoad(blockID, func(interface{}) (interface{}, error) {
		body, err := c.getBlockBody(blockID)
		if err != nil {
			return nil, err
		}
		ids := make(map[thor.Hash]int)
		for i, tx := range body.Txs {
			ids[tx.ID()] = i
		}
		return ids, nil
	})
	if err != nil {
		return nil, err
	}
	return ids.(map[thor.Hash]int), nil
}

// LookupTransaction find out the location of a tx, on the chain which ends with blockID.
func (c *Chain) LookupTransaction(blockID thor.Hash, txID thor.Hash) (*persist.TxLocation, error) {
	c.rw.RLock()
	defer c.rw.RUnlock()

	best, err := c.getBestBlock()
	if err != nil {
		return nil, err
	}
	from, err := c.getBlock(blockID)
	if err != nil {
		return nil, err
	}
	ancestor, branch, _, err := c.traceBackToCommonAncestor(from, best)
	if err != nil {
		return nil, err
	}
	for _, b := range branch {
		ids, err := c.getTransactionIDs(b.Header().ID())
		if err != nil {
			return nil, err
		}
		if index, found := ids[txID]; found {
			return &persist.TxLocation{
				BlockID: b.Header().ID(),
				Index:   uint64(index),
			}, nil
		}
	}
	loc, err := persist.LoadTxLocation(c.kv, txID)
	if err != nil {
		return nil, err
	}
	if block.Number(loc.BlockID) <= ancestor.Header().Number() {
		return loc, nil
	}
	return nil, errNotFound
}

// SetBlockReceipts set tx receipts of a block.
func (c *Chain) SetBlockReceipts(blockID thor.Hash, receipts tx.Receipts) error {
	c.rw.Lock()
	defer c.rw.Unlock()
	return c.setBlockReceipts(blockID, receipts)
}

func (c *Chain) setBlockReceipts(blockID thor.Hash, receipts tx.Receipts) error {
	c.cached.receipts.Add(blockID, receipts)
	return persist.SaveBlockReceipts(c.kv, blockID, receipts)
}

// GetBlockReceipts get tx receipts of a block.
func (c *Chain) GetBlockReceipts(blockID thor.Hash) (tx.Receipts, error) {
	c.rw.RLock()
	defer c.rw.RUnlock()
	return c.getBlockReceipts(blockID)
}

func (c *Chain) getBlockReceipts(blockID thor.Hash) (tx.Receipts, error) {
	receipts, err := c.cached.receipts.GetOrLoad(blockID, func(interface{}) (interface{}, error) {
		return persist.LoadBlockReceipts(c.kv, blockID)
	})
	if err != nil {
		return nil, err
	}
	return receipts.(tx.Receipts), nil
}

// GetTransactionReceipt get receipt for given tx ID.
func (c *Chain) GetTransactionReceipt(txID thor.Hash) (*tx.Receipt, error) {
	c.rw.RLock()
	defer c.rw.RUnlock()
	return c.getTransactionReceipt(txID)
}

func (c *Chain) getTransactionReceipt(txID thor.Hash) (*tx.Receipt, error) {
	_, loc, err := c.getTransaction(txID)
	if err != nil {
		return nil, err
	}
	receipts, err := c.getBlockReceipts(loc.BlockID)
	if err != nil {
		return nil, err
	}
	return receipts[loc.Index], nil
}

// IsNotFound returns if an error means not found.
func (c *Chain) IsNotFound(err error) bool {
	return err == errNotFound || c.kv.IsNotFound(err)
}

// NewTraverser create a block traverser to access blocks on the chain defined by headID.
func (c *Chain) NewTraverser(headID thor.Hash) *Traverser {
	return &Traverser{headID, c, nil}
}
