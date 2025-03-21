// Copyright Fuzamei Corp. 2018 All Rights Reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package mempool

import (
	"bytes"
	"encoding/hex"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/33cn/chain33/client"

	"github.com/33cn/chain33/common"
	log "github.com/33cn/chain33/common/log/log15"
	"github.com/33cn/chain33/queue"
	"github.com/33cn/chain33/types"
)

var mlog = log.New("module", "mempool.base")

//Mempool mempool 基础类
type Mempool struct {
	proxyMtx          sync.RWMutex
	in                chan *queue.Message
	out               <-chan *queue.Message
	client            queue.Client
	api               client.QueueProtocolAPI
	header            *types.Header
	sync              bool
	cfg               *types.Mempool
	poolHeader        chan struct{}
	isclose           int32
	wg                sync.WaitGroup
	done              chan struct{}
	removeBlockTicket *time.Ticker
	cache             *txCache
	delayTxListChan   chan []*types.Transaction
	currHeight        int64
}

func (mem *Mempool) setAPI(api client.QueueProtocolAPI) {
	mem.proxyMtx.Lock()
	mem.api = api
	mem.proxyMtx.Unlock()
}

func (mem *Mempool) getAPI() client.QueueProtocolAPI {
	mem.proxyMtx.RLock()
	defer mem.proxyMtx.RUnlock()
	return mem.api
}

//GetSync 判断是否mempool 同步
func (mem *Mempool) getSync() bool {
	mem.proxyMtx.RLock()
	defer mem.proxyMtx.RUnlock()
	return mem.sync
}

//NewMempool 新建mempool 实例
func NewMempool(cfg *types.Mempool) *Mempool {
	pool := &Mempool{}
	if cfg.MaxTxNumPerAccount == 0 {
		cfg.MaxTxNumPerAccount = maxTxNumPerAccount
	}
	if cfg.MaxTxLast == 0 {
		cfg.MaxTxLast = maxTxLast
	}
	if cfg.PoolCacheSize == 0 {
		cfg.PoolCacheSize = poolCacheSize
	}
	pool.in = make(chan *queue.Message)
	pool.out = make(<-chan *queue.Message)
	pool.done = make(chan struct{})
	pool.cfg = cfg
	pool.poolHeader = make(chan struct{}, 2)
	pool.removeBlockTicket = time.NewTicker(time.Minute)
	pool.cache = newCache(cfg.MaxTxNumPerAccount, cfg.MaxTxLast, cfg.PoolCacheSize)
	pool.delayTxListChan = make(chan []*types.Transaction, 16)

	return pool
}

//Close 关闭mempool
func (mem *Mempool) Close() {
	if mem.isClose() {
		return
	}
	atomic.StoreInt32(&mem.isclose, 1)
	close(mem.done)
	if mem.client != nil {
		mem.client.Close()
	}
	mem.removeBlockTicket.Stop()
	mlog.Info("mempool module closing")
	mem.wg.Wait()
	mlog.Info("mempool module closed")
}

//SetQueueClient 初始化mempool模块
func (mem *Mempool) SetQueueClient(cli queue.Client) {
	mem.client = cli
	mem.client.Sub("mempool")
	api, err := client.New(cli, nil)
	if err != nil {
		panic("Mempool SetQueueClient client.New err")
	}
	mem.setAPI(api)
	mem.wg.Add(1)
	go mem.pollLastHeader()
	mem.wg.Add(1)
	go mem.checkSync()
	mem.wg.Add(1)
	go mem.removeBlockedTxs()

	mem.wg.Add(1)
	go mem.eventProcess()
	go mem.pushDelayTxRoutine()
}

// Size 返回mempool中txCache大小
func (mem *Mempool) Size() int {
	mem.proxyMtx.RLock()
	defer mem.proxyMtx.RUnlock()
	return mem.cache.Size()
}

// SetMinFee 设置最小交易费用
func (mem *Mempool) SetMinFee(fee int64) {
	mem.proxyMtx.Lock()
	mem.cfg.MinTxFeeRate = fee
	mem.proxyMtx.Unlock()
}

//SetQueueCache 设置排队策略
func (mem *Mempool) SetQueueCache(qcache QueueCache) {
	mem.cache.SetQueueCache(qcache)
}

// GetTxList 从txCache中返回给定数目的tx
func (mem *Mempool) getTxList(filterList *types.TxHashList) (txs []*types.Transaction) {
	mem.proxyMtx.Lock()
	defer mem.proxyMtx.Unlock()
	count := filterList.GetCount()
	dupMap := make(map[string]bool)
	for i := 0; i < len(filterList.GetHashes()); i++ {
		dupMap[string(filterList.GetHashes()[i])] = true
	}
	return mem.filterTxList(count, dupMap, false)
}

func (mem *Mempool) filterTxList(count int64, dupMap map[string]bool, isAll bool) (txs []*types.Transaction) {
	//mempool中的交易都是未打包的，需要用下一个区块的高度和时间作为交易过期判定
	height := mem.header.GetHeight() + 1
	blockTime := mem.header.GetBlockTime()
	types.AssertConfig(mem.client)
	cfg := mem.client.GetConfig()
	//由于mempool可能存在过期交易，先遍历所有，满足目标交易数再退出，否则存在无法获取到实际交易情况
	mem.cache.Walk(0, func(tx *Item) bool {
		if len(dupMap) > 0 {
			if _, ok := dupMap[string(tx.Value.Hash())]; ok {
				return true
			}
		}
		if isExpired(cfg, tx, height, blockTime) && !isAll {
			return true
		}
		txs = append(txs, tx.Value)
		//达到设定的交易数，退出循环, count为0获取所有
		if count > 0 && len(txs) == int(count) {
			return false
		}
		return true
	})

	if mem.client.GetConfig().IsFork(mem.header.GetHeight(), "ForkCheckEthTxSort") {
		//对txs 进行排序
		txs = mem.sortEthSignTyTx(txs)
	}
	return txs
}

//对eth signtype 的交易，同地址下nonce 按照从小到达的顺序排序
//确保nonce 按照递增顺序发给blockchain
func (mem *Mempool) sortEthSignTyTx(txs []*types.Transaction) []*types.Transaction {
	//平行链架构下，主链节点无法获取到平行链evm的nonce
	var merge []*types.Transaction
	var ethsignTxs = make(map[string][]*types.Transaction)
	for _, tx := range txs {
		//只有eth 签名且非平行链交易才能进入mempool 中进行 nonce 排序
		if types.IsEthSignID(tx.GetSignature().GetTy()) && !bytes.HasPrefix(tx.GetExecer(), []byte(types.ParaKeyX)) {
			//暂时不考虑组交易的情况
			ethsignTxs[tx.From()] = append(ethsignTxs[tx.From()], tx)
			continue
		}
		//非eth 签名 和 平行链交易 在主网节点中直接返回给blockchain,因为主网节点不知道此tx.From地址在主网节点的nonce 状态，没法排序，只能在平行链节点rpc层过滤掉
		merge = append(merge, tx)
	}
	//没有ethsign 交易直接返回
	if len(merge) == len(txs) {
		return txs
	}

	//sort
	for from, etxs := range ethsignTxs {
		sort.SliceStable(etxs, func(i, j int) bool { //nonce asc
			return etxs[i].GetNonce() < etxs[j].GetNonce()
		})
		//check exts[0].Nonce 是否等于current nonce, merge
		if len(etxs) != 0 && mem.getCurrentNonce(from) == etxs[0].GetNonce() {
			merge = append(merge, etxs[0])
			for i, etx := range etxs {
				if i == 0 {
					continue
				}
				//要求nonce 具有连续性
				if etx.GetNonce() == etxs[i-1].GetNonce()+1 {
					merge = append(merge, etxs[i])
					continue
				}
				break
			}
		}
	}

	return merge
}

func (mem *Mempool) getCurrentNonce(addr string) int64 {
	msg := mem.client.NewMessage("rpc", types.EventGetEvmNonce, &types.ReqEvmAccountNonce{
		Addr: addr,
	})
	mem.client.Send(msg, true)
	reply, err := mem.client.WaitTimeout(msg, time.Second*2)
	if err == nil {
		nonceInfo, ok := reply.GetData().(*types.EvmAccountNonce)
		if ok {
			return nonceInfo.GetNonce()
		}
	}
	return 0

}

// RemoveTxs 从mempool中删除给定Hash的txs
func (mem *Mempool) RemoveTxs(hashList *types.TxHashList) error {
	mem.proxyMtx.Lock()
	defer mem.proxyMtx.Unlock()
	mem.removeTxs(hashList.Hashes)
	return nil
}

func (mem *Mempool) removeTxs(hashes [][]byte) {

	for _, hash := range hashes {
		exist := mem.cache.Exist(string(hash))
		if exist {
			mem.cache.Remove(string(hash))
		}
	}
}

// PushTx 将交易推入mempool，并返回结果（error）
func (mem *Mempool) PushTx(tx *types.Transaction) error {
	mem.proxyMtx.Lock()
	defer mem.proxyMtx.Unlock()
	err := mem.cache.Push(tx)
	return err
}

//  setHeader设置mempool.header
func (mem *Mempool) setHeader(h *types.Header) {
	atomic.StoreInt64(&mem.currHeight, h.Height)
	mem.proxyMtx.Lock()
	mem.header = h
	mem.proxyMtx.Unlock()
}

// GetHeader 获取header, 只需要读锁
func (mem *Mempool) GetHeader() *types.Header {
	mem.proxyMtx.RLock()
	defer mem.proxyMtx.RUnlock()
	return mem.header
}

//IsClose 判断是否mempool 关闭
func (mem *Mempool) isClose() bool {
	return atomic.LoadInt32(&mem.isclose) == 1
}

// GetLastHeader 获取LastHeader的height和blockTime
func (mem *Mempool) GetLastHeader() (interface{}, error) {
	if mem.client == nil {
		panic("client not bind message queue.")
	}
	msg := mem.client.NewMessage("blockchain", types.EventGetLastHeader, nil)
	err := mem.client.Send(msg, true)
	if err != nil {
		mlog.Error("blockchain closed", "err", err.Error())
		return nil, err
	}
	return mem.client.Wait(msg)
}

// GetAccTxs 用来获取对应账户地址（列表）中的全部交易详细信息
func (mem *Mempool) GetAccTxs(addrs *types.ReqAddrs) *types.TransactionDetails {
	mem.proxyMtx.Lock()
	defer mem.proxyMtx.Unlock()
	return mem.cache.GetAccTxs(addrs)
}

// TxNumOfAccount 返回账户在mempool中交易数量
func (mem *Mempool) TxNumOfAccount(addr string) int64 {
	mem.proxyMtx.Lock()
	defer mem.proxyMtx.Unlock()
	return int64(mem.cache.TxNumOfAccount(addr))
}

// GetLatestTx 返回最新十条加入到mempool的交易
func (mem *Mempool) GetLatestTx() []*types.Transaction {
	mem.proxyMtx.Lock()
	defer mem.proxyMtx.Unlock()
	return mem.cache.GetLatestTx()
}

// GetTotalCacheBytes 获取缓存交易的总占用空间
func (mem *Mempool) GetTotalCacheBytes() int64 {
	mem.proxyMtx.Lock()
	defer mem.proxyMtx.Unlock()
	return mem.cache.qcache.GetCacheBytes()
}

// pollLastHeader 在初始化后循环获取LastHeader，直到获取成功后，返回
func (mem *Mempool) pollLastHeader() {
	defer mem.wg.Done()
	defer func() {
		mlog.Info("pollLastHeader quit")
		mem.poolHeader <- struct{}{}
	}()
	for {
		if mem.isClose() {
			return
		}
		lastHeader, err := mem.GetLastHeader()
		if err != nil {
			mlog.Error(err.Error())
			time.Sleep(time.Second)
			continue
		}
		h := lastHeader.(*queue.Message).Data.(*types.Header)
		mem.setHeader(h)
		return
	}
}

func (mem *Mempool) removeExpired() {
	mem.proxyMtx.Lock()
	defer mem.proxyMtx.Unlock()
	types.AssertConfig(mem.client)
	//mempool的header是当前高度，而交易将被下一个区块打包，过期判定采用下一个区块的高度和时间
	mem.cache.removeExpiredTx(mem.client.GetConfig(), mem.header.GetHeight()+1, mem.header.GetBlockTime())
}

// removeBlockedTxs 每隔1分钟清理一次已打包的交易
func (mem *Mempool) removeBlockedTxs() {
	defer mem.wg.Done()
	defer mlog.Info("RemoveBlockedTxs quit")
	if mem.client == nil {
		panic("client not bind message queue.")
	}
	for {
		select {
		case <-mem.removeBlockTicket.C:
			if mem.isClose() {
				return
			}
			mem.removeExpired()
		case <-mem.done:
			return
		}
	}
}

// RemoveTxsOfBlock 移除mempool中已被Blockchain打包的tx
func (mem *Mempool) RemoveTxsOfBlock(block *types.Block) bool {
	mem.proxyMtx.Lock()
	defer mem.proxyMtx.Unlock()
	for _, tx := range block.Txs {
		hash := tx.Hash()
		exist := mem.cache.Exist(string(hash))
		if exist {
			mem.cache.Remove(string(hash))
		}
	}
	return true
}
func (mem *Mempool) getCacheFeeRate() int64 {
	if mem.cache.qcache == nil {
		return 0
	}
	feeRate := mem.cache.qcache.GetProperFee()

	//控制精度
	unitFee := mem.cfg.MinTxFeeRate
	if unitFee != 0 && feeRate%unitFee > 0 {
		feeRate = (feeRate/unitFee + 1) * unitFee
	}
	if feeRate > mem.cfg.MaxTxFeeRate {
		feeRate = mem.cfg.MaxTxFeeRate
	}
	return feeRate
}

// GetProperFeeRate 获取合适的手续费率
func (mem *Mempool) GetProperFeeRate(req *types.ReqProperFee) int64 {
	if req == nil || req.TxCount == 0 {
		req = &types.ReqProperFee{TxCount: 20}
	}
	if req.TxSize == 0 {
		req.TxSize = 10240
	}
	feeRate := mem.getCacheFeeRate()
	if mem.cfg.IsLevelFee {
		levelFeeRate := mem.getLevelFeeRate(mem.cfg.MinTxFeeRate, req.TxCount, req.TxSize)
		if levelFeeRate > feeRate {
			feeRate = levelFeeRate
		}
	}
	return feeRate
}

// getLevelFeeRate 获取合适的阶梯手续费率, 可以外部传入count, size进行前瞻性估计
func (mem *Mempool) getLevelFeeRate(baseFeeRate int64, appendCount, appendSize int32) int64 {
	var feeRate int64
	sumByte := mem.GetTotalCacheBytes() + int64(appendSize)
	types.AssertConfig(mem.client)
	cfg := mem.client.GetConfig()
	maxTxNumber := cfg.GetP(mem.Height()).MaxTxNumber
	memSize := mem.Size()
	switch {
	case sumByte >= int64(types.MaxBlockSize/20) || int64(memSize+int(appendCount)) >= maxTxNumber/2:
		feeRate = 100 * baseFeeRate
	case sumByte >= int64(types.MaxBlockSize/100) || int64(memSize+int(appendCount)) >= maxTxNumber/10:
		feeRate = 10 * baseFeeRate
	default:
		feeRate = baseFeeRate
	}
	if feeRate > mem.cfg.MaxTxFeeRate {
		feeRate = mem.cfg.MaxTxFeeRate
	}
	return feeRate
}

// Mempool.DelBlock将回退的区块内的交易重新加入mempool中
func (mem *Mempool) delBlock(block *types.Block) {
	if len(block.Txs) <= 0 {
		return
	}
	blkTxs := block.Txs
	types.AssertConfig(mem.client)
	cfg := mem.client.GetConfig()
	for i := 0; i < len(blkTxs); i++ {
		tx := blkTxs[i]
		//当前包括ticket和平行链的第一笔挖矿交易，统一actionName为miner
		if i == 0 && tx.ActionName() == types.MinerAction {
			continue
		}
		groupCount := int(tx.GetGroupCount())
		if groupCount > 1 && i+groupCount <= len(blkTxs) {
			group := types.Transactions{Txs: blkTxs[i : i+groupCount]}
			tx = group.Tx()
			i = i + groupCount - 1
		}
		err := tx.Check(cfg, mem.GetHeader().GetHeight(), mem.cfg.MinTxFeeRate, mem.cfg.MaxTxFee)
		if err != nil {
			continue
		}
		if !mem.checkExpireValid(tx) {
			continue
		}
		err = mem.PushTx(tx)
		if err != nil {
			mlog.Error("mem", "push tx err", err)
		}
	}
}

// Height 获取区块高度
func (mem *Mempool) Height() int64 {
	mem.proxyMtx.Lock()
	defer mem.proxyMtx.Unlock()
	if mem.header == nil {
		return -1
	}
	return mem.header.GetHeight()
}

// Wait wait mempool ready
func (mem *Mempool) Wait() {
	<-mem.poolHeader
	//wait sync
	<-mem.poolHeader
}

// SendTxToP2P 向"p2p"发送消息
func (mem *Mempool) sendTxToP2P(tx *types.Transaction) {
	if mem.client == nil {
		panic("client not bind message queue.")
	}
	msg := mem.client.NewMessage("p2p", types.EventTxBroadcast, tx)
	err := mem.client.Send(msg, false)
	if err != nil {
		mlog.Error("tx sent to p2p", "tx.Hash", common.ToHex(tx.Hash()))
		return
	}
	//mlog.Debug("tx sent to p2p", "tx.Hash", common.ToHex(tx.Hash()))
}

// Mempool.checkSync检查并获取mempool同步状态
func (mem *Mempool) checkSync() {
	defer func() {
		mlog.Info("getsync quit")
		mem.poolHeader <- struct{}{}
	}()
	defer mem.wg.Done()
	if mem.getSync() {
		return
	}
	if mem.cfg.ForceAccept {
		mem.setSync(true)
	}
	for {
		if mem.isClose() {
			return
		}
		if mem.client == nil {
			panic("client not bind message queue.")
		}
		msg := mem.client.NewMessage("blockchain", types.EventIsSync, nil)
		err := mem.client.Send(msg, true)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		resp, err := mem.client.Wait(msg)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		if resp.GetData().(*types.IsCaughtUp).GetIscaughtup() {
			mem.setSync(true)
			// 通知p2p广播模块，区块同步状态
			err = mem.client.Send(mem.client.NewMessage("p2p", types.EventIsSync, nil), false)
			if err != nil {
				mlog.Error("checkSync", "send p2p error", err)
			}
			return
		}
		time.Sleep(time.Second)
		continue
	}
}

func (mem *Mempool) setSync(status bool) {
	mem.proxyMtx.Lock()
	mem.sync = status
	mem.proxyMtx.Unlock()
}

// getTxListByHash 从qcache或者SHashTxCache中获取hash对应的tx交易列表
func (mem *Mempool) getTxListByHash(hashList *types.ReqTxHashList) *types.ReplyTxList {
	mem.proxyMtx.Lock()
	defer mem.proxyMtx.Unlock()

	var replyTxList types.ReplyTxList

	//通过短hash来获取tx交易
	if hashList.GetIsShortHash() {
		for _, sHash := range hashList.GetHashes() {
			tx := mem.cache.GetSHashTxCache(sHash)
			replyTxList.Txs = append(replyTxList.Txs, tx)
		}
		return &replyTxList
	}
	//通过hash来获取tx交易
	for _, hash := range hashList.GetHashes() {
		tx := mem.cache.getTxByHash(hash)
		replyTxList.Txs = append(replyTxList.Txs, tx)
	}
	return &replyTxList
}

// push expired delay tx to mempool
func (mem *Mempool) pushDelayTxRoutine() {

	retryList := make([]*types.Transaction, 0, 8)
	push2Mempool := func(tx *types.Transaction) {
		_, err := mem.getAPI().SendTx(tx)
		if err != nil {
			mlog.Error("sendDelayTx", "height", mem.Height(), "txHash", hex.EncodeToString(tx.Hash()), "send tx err", err)
		} else {
			mlog.Info("sendDelayTx", "txHash", hex.EncodeToString(tx.Hash()))
		}
		// try later if mempool is full
		if err == types.ErrMemFull {
			retryList = append(retryList, tx)
		}
	}
	ticker := time.NewTicker(time.Second)

	for {

		select {
		case <-mem.done:
			ticker.Stop()
			return
		case delayList := <-mem.delayTxListChan:
			for _, tx := range delayList {
				push2Mempool(tx)
			}
		case <-ticker.C:
			if len(retryList) == 0 {
				break
			}
			// retry send to mempool
			sendList := retryList
			retryList = make([]*types.Transaction, 0, 8)
			for _, tx := range sendList {
				push2Mempool(tx)
			}
		}
	}
}
