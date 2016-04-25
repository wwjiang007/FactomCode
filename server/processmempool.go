// Copyright 2015 FactomProject Authors. All rights reserved.
// Use of this source code is governed by the MIT license
// that can be found in the LICENSE file.

package server

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/FactomProject/FactomCode/common"
	"github.com/FactomProject/FactomCode/wire"
	"github.com/FactomProject/factoid/block"
	"github.com/davecgh/go-spew/spew"
)

// ftmMemPool is used as a source of factom transactions
// (CommitChain, RevealChain, CommitEntry, RevealEntry, Ack, EOM, DirBlockSig)
type ftmMemPool struct {
	sync.RWMutex
	pool         map[wire.ShaHash]wire.Message
	orphans      map[wire.ShaHash]wire.Message
	blockpool    map[string]wire.Message // to hold the blocks or entries downloaded from peers
	ackpool      []*wire.MsgAck
	dirBlockSigs []*wire.MsgDirBlockSig
	requested		map[common.Hash]*reqMsg
	lastUpdated  time.Time // last time pool was updated
}

type reqMsg struct {
	Msg *wire.MsgGetFactomData
	TimesMissed uint32
	Requested bool
}

// Add a factom message to the orphan pool
func (mp *ftmMemPool) initFtmMemPool() error {
	mp.pool = make(map[wire.ShaHash]wire.Message)
	mp.orphans = make(map[wire.ShaHash]wire.Message)
	mp.blockpool = make(map[string]wire.Message)
	mp.ackpool = make([]*wire.MsgAck, 100, 200)
	mp.dirBlockSigs = make([]*wire.MsgDirBlockSig, 0, 32)
	mp.requested = make(map[common.Hash]*reqMsg)
	
	return nil
}

func (mp *ftmMemPool) addMissingMsg(typ wire.InvType, hash *common.Hash, height uint32) *reqMsg {
	h := common.NewHashFromByte(hash.ByteArray())
	mp.RLock()
	if m, ok := mp.requested[h]; ok {
		m.TimesMissed++
		mp.RUnlock()
		return m
	}
	mp.RUnlock()
	
	inv := &wire.InvVectHeight {
		Type: typ,
		Hash: h,
		Height: int64(height),
	}
	fd := wire.NewMsgGetFactomData()
	fd.AddInvVectHeight(inv)
	
	req := &reqMsg {
		Msg: fd,
		TimesMissed: 1,
	}
	fmt.Println("request pool: addMissingMsg: ", spew.Sdump(req))
	mp.Lock()
	mp.requested[h] = req
	mp.Unlock()
	return req
}

func (mp *ftmMemPool) removeMissingMsg(hash *common.Hash) {
	mp.Lock()
	defer mp.Unlock()

	h := common.NewHashFromByte(hash.ByteArray())
	if _, ok := mp.requested[h]; ok {
		fmt.Println("request pool: removeMissingMsg: ", hash.String())
		delete(mp.requested, h)
	}
}

func (mp *ftmMemPool) FetchFromBlockpool(str string) wire.Message {
	mp.RLock()
	defer mp.RUnlock()

	return mp.blockpool[str]
}

func (mp *ftmMemPool) FetchAndFoundFromBlockpool(str string) (wire.Message, bool) {
	mp.RLock()
	defer mp.RUnlock()

	msg, found := mp.blockpool[str]
	return msg, found
}

func (mp *ftmMemPool) addDirBlockSig(dbsig *wire.MsgDirBlockSig) {
	mp.Lock()
	defer mp.Unlock()

	mp.dirBlockSigs = append(mp.dirBlockSigs, dbsig)
}

func (mp *ftmMemPool) lenDirBlockSig() int {
	mp.RLock()
	defer mp.RUnlock()

	return len(mp.dirBlockSigs)
}

func (mp *ftmMemPool) getDirBlockSigPool() []*wire.MsgDirBlockSig {
	mp.RLock()
	defer mp.RUnlock()

	return mp.dirBlockSigs
}

func (mp *ftmMemPool) resetDirBlockSigPool(height uint32) {
	mp.Lock()
	defer mp.Unlock()

	fmt.Println("resetDirBlockSigPool, height=", height)
	a := mp.dirBlockSigs
	i := 0
	for i < len(a) {
		if a[i].DBHeight <= height {
			a, a[len(a)-1] = append(a[:i], a[i+1:]...), nil
		} else {
			i++
		}
	}
}

func (mp *ftmMemPool) resetAckPool() {
	mp.Lock()
	defer mp.Unlock()

	fmt.Println("resetAckPool")
	mp.ackpool = make([]*wire.MsgAck, 100, 200)
}

// addAck add the ack to ackpool and find it's acknowledged msg.
// then add them to ftmMemPool if available.
// otherwise return missing acked msg.
func (mp *ftmMemPool) addAck(ack *wire.MsgAck) *wire.MsgMissing {
	if ack == nil {
		return nil
	}

	mp.Lock()
	defer mp.Unlock()

	// reset it at the very beginning
	if ack.Index == 0 {
		mp.ackpool = make([]*wire.MsgAck, 100, 200)
		fmt.Println("reset ackpool")
	} else if ack.Index == uint32(len(mp.ackpool)) {
		mp.ackpool = mp.ackpool[0 : ack.Index+50]
		fmt.Println("grow ackpool.len by 50")
	} else if ack.Index == uint32(cap(mp.ackpool)) {
		temp := make([]*wire.MsgAck, ack.Index*2, ack.Index*2)
		copy(temp, mp.ackpool)
		mp.ackpool = temp
		fmt.Println("double ackpool capacity")
	}

	// check duplication first
	a := mp.ackpool[ack.Index]
	if a != nil && ack.Equals(a) {
		fmt.Println("duplicated ack: ignore it")
		return nil
	}

	fmt.Printf("ftmMemPool.addAck: %s\n", ack)
	mp.ackpool[ack.Index] = ack
	if ack.Type == wire.AckRevealEntry || ack.Type == wire.AckRevealChain ||
		ack.Type == wire.AckCommitChain || ack.Type == wire.AckCommitEntry ||
		ack.Type == wire.AckFactoidTx {
		msg := mp.pool[*ack.Affirmation]
		if msg == nil {
			// missing msg and request it from the leader ???
			// create a new msg type
			miss := wire.NewMsgMissing(ack.Height, ack.Index, ack.Type, false, *ack.Affirmation, localServer.nodeID)
			miss.Sig = serverPrivKey.Sign(miss.GetBinaryForSignature())
			return miss
		}
	}
	return nil
}

// this ack is always an EOM type, and check missing acks for this minute only.
func (mp *ftmMemPool) getMissingMsgAck(ack *wire.MsgAck) []*wire.MsgMissing {
	var missingAcks []*wire.MsgMissing
	if ack.Index == 0 {
		return missingAcks
	}

	mp.RLock()
	mp.RUnlock()

	for i := int(ack.Index - 1); i >= 0; i-- {
		if mp.ackpool[uint32(i)] == nil {
			// missing an ACK here.
			m := wire.NewMsgMissing(ack.Height, uint32(i), wire.Unknown, true, zeroBtcHash, localServer.nodeID)
			m.Sig = localServer.privKey.Sign(m.GetBinaryForSignature())
			missingAcks = append(missingAcks, m)
			fmt.Printf("ftmMemPool.getMissingMsgAck: Missing an Ack at index=%d\n", i)
		} else if mp.ackpool[uint32(i)].IsEomAck() {
			// this ack is an EOM and here we found its previous EOM
			break
		}
	}
	return missingAcks
}

func (mp *ftmMemPool) assembleFollowerProcessList(ack *wire.MsgAck) error {
	mp.RLock()
	defer mp.RUnlock()

	// simply validation
	if ack.Type != wire.EndMinute10 || mp.ackpool[ack.Index] != ack {
		return fmt.Errorf("the last ack has to be EndMinute10")
	}
	var msg wire.Message
	var hash *wire.ShaHash
	for i := 0; i < len(mp.ackpool); i++ {
		if mp.ackpool[i] == nil {
			// missing an ACK here
			// todo: it might be too late to request for this ack ???
			fmt.Printf("ERROR: assembleFollowerProcessList: Missing an Ack in ackpool at index=%d\n", i)
			continue
		}
		if mp.ackpool[i].Affirmation != nil {
			msg = mp.pool[*mp.ackpool[i].Affirmation]
		}
		if msg == nil && !mp.ackpool[i].IsEomAck() {
			fmt.Printf("ERROR: assembleFollowerProcessList: Missing a MSG in pool at index=%d\n", i)
			continue
		}
		if msg == nil {
			hash = nil
		} else {
			hash = mp.ackpool[i].Affirmation
		}
		plMgr.AddToFollowersProcessList(msg, mp.ackpool[i], hash)
		if mp.ackpool[i].Type == wire.EndMinute10 {
			break
		}
	}
	return nil
}

func (mp *ftmMemPool) haveMsg(hash wire.ShaHash) bool {
	mp.RLock()
	defer mp.RUnlock()

	m := mp.pool[hash]
	if m != nil {
		fmt.Println("hasMsg: hash=", hex.EncodeToString(hash.Bytes()))
		return true
	}
	return false
}

func (mp *ftmMemPool) getMsg(hash wire.ShaHash) wire.Message {
	mp.RLock()
	defer mp.RUnlock()

	return mp.pool[hash]
}

// Add a factom message to the  Mem pool
func (mp *ftmMemPool) addMsg(msg wire.Message, hash *wire.ShaHash) error {
	mp.Lock()
	defer mp.Unlock()

	if len(mp.pool) > common.MAX_TX_POOL_SIZE {
		return errors.New("Transaction mem pool exceeds the limit.")
	}
	// todo: should check if exists, then just pass and no need to add it.
	mp.pool[*hash] = msg
	return nil
}

// Add a factom message to the orphan pool
func (mp *ftmMemPool) addOrphanMsg(msg wire.Message, hash *wire.ShaHash) error {
	mp.Lock()
	defer mp.Unlock()

	if len(mp.orphans) > common.MAX_ORPHAN_SIZE {
		return errors.New("Ophan mem pool exceeds the limit.")
	}
	mp.orphans[*hash] = msg
	return nil
}

// Add a factom block message to the  Mem pool
func (mp *ftmMemPool) addBlockMsg(msg wire.Message, hash string) error {
	mp.Lock()
	defer mp.Unlock()

	// todo: should check if exists, then just pass and no need to add it.
	if len(mp.blockpool) > common.MAX_BLK_POOL_SIZE {
		return errors.New("Block mem pool exceeds the limit. Please restart.")
	}
	//fmt.Println("ftmMemPool.addBlockMsg: msg.Command=", msg.Command())
	mp.blockpool[hash] = msg
	return nil
}

// Delete a factom block message from the  Mem pool
func (mp *ftmMemPool) deleteBlockMsg(hash string) error {
	mp.Lock()
	defer mp.Unlock()

	if mp.blockpool[hash] != nil {
		delete(mp.blockpool, hash)
	}
	return nil
}

func (mp *ftmMemPool) haveDirBlock() bool {
	for _, v := range mp.blockpool {
		if v.Command() == wire.CmdDirBlock {
			return true
		}
	}
	return false
}

//getDirBlock return a dir block by height
func (mp *ftmMemPool) getDirBlock(height uint32) *common.DirectoryBlock {
	mp.RLock()
	defer mp.RUnlock()

	h := strconv.Itoa(int(height))
	msg := mp.blockpool[h]
	fmt.Println("ftmMemPool.getDirBlock: height=", h)
	if msg != nil && msg.Command() == wire.CmdDirBlock {
		dirBlockMsg, _ := msg.(*wire.MsgDirBlock)
		return dirBlockMsg.DBlk
	}
	return nil
}

func (mp *ftmMemPool) getFBlock(height uint32) block.IFBlock {
	mp.RLock()
	defer mp.RUnlock()

	for _, msg := range mp.blockpool {
		fmt.Println(msg.Command())
		if msg.Command() == wire.CmdFBlock {
			fBlockMsg, _ := msg.(*wire.MsgFBlock)
			if fBlockMsg.SC.GetDBHeight() == height {
				return fBlockMsg.SC
			}
		}
	}
	return nil
}

func (mp *ftmMemPool) getECBlock(height uint32) *common.ECBlock {
	mp.RLock()
	defer mp.RUnlock()

	for _, msg := range mp.blockpool {
		fmt.Println(msg.Command())
		if msg.Command() == wire.CmdECBlock {
			ecBlockMsg, _ := msg.(*wire.MsgECBlock)
			if ecBlockMsg.ECBlock.Header.EBHeight == height {
				return ecBlockMsg.ECBlock
			}
		}
	}
	return nil
}

func (mp *ftmMemPool) getABlock(height uint32) *common.AdminBlock {
	mp.RLock()
	defer mp.RUnlock()

	for _, msg := range mp.blockpool {
		fmt.Println(msg.Command())
		if msg.Command() == wire.CmdABlock {
			aBlockMsg, _ := msg.(*wire.MsgABlock)
			if aBlockMsg.ABlk.Header.DBHeight == height {
				return aBlockMsg.ABlk
			}
		}
	}
	return nil
}

func (mp *ftmMemPool) getEBlock(height uint32) *common.EBlock {
	mp.RLock()
	defer mp.RUnlock()

	for _, msg := range mp.blockpool {
		fmt.Println(msg.Command())
		if msg.Command() == wire.CmdEBlock {
			eBlockMsg, _ := msg.(*wire.MsgEBlock)
			if eBlockMsg.EBlk.Header.EBSequence == height {
				return eBlockMsg.EBlk
			}
		}
	}
	return nil
}

// after blocks being built, remove messages included in the process list
func (mp *ftmMemPool) cleanUpMemPool() {
	mp.Lock()
	defer mp.Unlock()

	plItems := plMgr.MyProcessList.GetPLItems()
	for _, item := range plItems {
		if item == nil {
			continue
		}
		if item.MsgHash != nil {
			fmt.Println("cleanUpMemPool: ", item.MsgHash.String())
			delete(mp.pool, *item.MsgHash)
		}
	}
}

// relay stale messages left in mempool and orphan pool.
func (mp *ftmMemPool) relayStaleMessages() {
	mp.Lock()
	defer mp.Unlock()

	for hash, msg := range mp.pool {
		fmt.Println("relayStaleMessages: from mp.pool: ", spew.Sdump(msg))
		outMsgQueue <- msg
		delete(mp.pool, hash)
	}
	for hash, msg := range mp.orphans {
		fmt.Println("relayStaleMessages: from mp.orphans: ", spew.Sdump(msg))
		outMsgQueue <- msg
		delete(mp.orphans, hash)
	}
}