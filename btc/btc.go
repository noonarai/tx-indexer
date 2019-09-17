package btc

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SwingbyProtocol/sc-indexer/resolver"
	"github.com/ant0ine/go-json-rest/rest"
	log "github.com/sirupsen/logrus"
	"github.com/syndtr/goleveldb/leveldb"
)

var (
	lock  = sync.RWMutex{}
	tasks = []string{}
)

type BTCNode struct {
	Index       map[string]string
	Spent       map[string]bool
	Updates     map[string]int64
	PruneBlocks int64
	LocalBlocks int64
	BestBlocks  int64
	Resolver    *resolver.Resolver
	URI         string
	db          *leveldb.DB
	Status      string
}

type Meta struct {
	Height int64
	Count  int
	Txs    []string
}

type ChainInfo struct {
	Chain         string `json:"chain"`
	Blocks        int64  `json:"blocks"`
	Headers       int64  `json:"headers"`
	BestBlockHash string `json:"bestblockhash"`
}

type Block struct {
	Hash              string `json:"hash"`
	Confirmations     int64  `json:"confirmations"`
	Height            int64  `json:"height"`
	NTx               int64  `json:"nTx"`
	Txs               []*Tx  `json:"tx"`
	Time              int64  `json:"time"`
	Mediantime        int64  `json:"mediantime"`
	Previousblockhash string `json:"previousblockhash"`
}

func NewBTCNode(uri string, pruneBlocks int64) *BTCNode {
	db, err := leveldb.OpenFile("./db", nil)
	if err != nil {
		log.Fatal(err)
	}
	node := &BTCNode{
		Index:       make(map[string]string),
		Spent:       make(map[string]bool),
		Updates:     make(map[string]int64),
		URI:         uri,
		PruneBlocks: pruneBlocks,
		Resolver:    resolver.NewResolver(),
		db:          db,
		Status:      "init",
	}
	return node
}

func (b *BTCNode) Start() {
	runTime := time.NewTicker(10 * time.Second)
	err := b.LoadData()
	if err != nil {
		log.Info(err)
	}
	go func() {
		b.Run()
		for {
			select {
			case <-runTime.C:
				go b.Run()
			}
		}
	}()
	taskTime := time.NewTicker(8 * time.Second)
	go func() {
		b.GetBlock()
		for {
			select {
			case <-taskTime.C:
				go b.GetBlock()
			}
		}
	}()
}

func (b *BTCNode) Run() error {
	res := ChainInfo{}
	err := b.Resolver.GetRequest(b.URI, "/rest/chaininfo.json", &res)
	if err != nil {
		return err
	}
	log.Info("call to bitcoind best blocks -> ", res.Blocks)
	if b.Status == "init" {
		b.LocalBlocks = res.Blocks - b.PruneBlocks
		b.BestBlocks = res.Blocks
		tasks = append(tasks, "0_"+res.BestBlockHash)
		b.Status = "start"
	} else {
		if b.Status == "loop" && b.BestBlocks != res.Blocks {
			b.BestBlocks = res.Blocks
			tasks = append(tasks, "0_"+res.BestBlockHash)
		}
	}
	return nil
}

func (b *BTCNode) GetBlock() error {
	if len(tasks) == 0 {
		return errors.New("no")
	}
	lock.Lock()
	task := tasks[0]
	tasks = tasks[1:]
	lock.Unlock()
	if task[:1] != "0" {
		return errors.New("task is not zero")
	}
	res := Block{}
	err := b.Resolver.GetRequest(b.URI, "/rest/block/"+task[2:]+".json", &res)
	if err != nil {
		tasks = append(tasks, task)
		return err
	}
	if res.Height == 0 {
		tasks = append(tasks, task)
		return errors.New("height is zero")
	}
	log.Infof("Fetch block -> %d", res.Height)
	txs := b.CheckAllSpentTx(&res)
	b.StoreTxs(res, txs)
	b.PutIndex(txs)
	b.DeleteIndex()
	for key := range b.Index {
		go b.showIndex(key)
	}
	if b.LocalBlocks+1 == res.Height {
		b.LocalBlocks = b.BestBlocks
		b.Status = "loop"
		return nil
	}
	if b.LocalBlocks < res.Height {
		tasks = append(tasks, "0_"+res.Previousblockhash)
	}
	return nil
}

func (b *BTCNode) showIndex(key string) {
	lock.RLock()
	txIDs := strings.Split(b.Index[key], "_")
	update := b.Updates[key]
	lock.RUnlock()
	if len(txIDs) > 3*int(b.PruneBlocks) {
		log.Infof("counts -> %12d updated -> %12d addr -> %s ", len(txIDs), update, key)
	}
}

func (b *BTCNode) CheckAllSpentTx(block *Block) []*Tx {
	vouts := make(map[string]int)
	for _, tx := range block.Txs {
		for _, vin := range tx.Vin {
			if len(vin.Txid) != 64 {
				continue
			}
			key := vin.Txid + "_" + strconv.Itoa(vin.Vout)
			b.Spent[key] = true
			go b.StoreSpent(key)
		}
		vouts[tx.Txid] = len(tx.Vout)
	}
	for key := range b.Spent {
		txID := key[0:64]
		for _, tx := range block.Txs {
			if txID == tx.Txid {
				vouts[tx.Txid] = vouts[tx.Txid] - 1
			}
		}
	}
	txs := []*Tx{}
	count := 0
	for _, tx := range block.Txs {
		if vouts[tx.Txid] != 0 {
			txs = append(txs, tx)
		} else {
			count++
		}
	}
	//log.Infof("removed -> %d", count)
	return txs
}

func (b *BTCNode) DeleteIndex() {
	for addr, date := range b.Updates {
		if b.BestBlocks-b.PruneBlocks > date {
			delete(b.Index, addr)
			go b.RemoveIndex(addr)
			delete(b.Updates, addr)
			go b.RemoveUpdate(addr)
			//log.Info("delete ->", addr)
		}
	}
}

func (b *BTCNode) PutIndex(txs []*Tx) {
	for _, tx := range txs {
		for _, vout := range tx.Vout {
			for _, addr := range vout.ScriptPubkey.Addresses {
				if b.Index[addr] == "" {
					b.Index[addr] = tx.Txid
				} else {
					txIDs := strings.Split(b.Index[addr], "_")
					isMatch := false
					for _, txID := range txIDs {
						if txID == tx.Txid {
							isMatch = true
						}
					}
					if isMatch == false {
						b.Index[addr] = b.Index[addr] + "_" + tx.Txid
						b.Updates[addr] = b.BestBlocks
						go b.StoreIndex(addr, b.Index[addr])
						go b.StoreUpdate(addr, int(b.Updates[addr]))
					}
				}
			}
		}
	}
}

func (b *BTCNode) StoreIndex(addr string, data string) error {
	err := b.db.Put([]byte("index_"+addr), []byte(data), nil)
	if err != nil {
		fmt.Printf("Error: %s", err)
		return err
	}
	return nil
}

func (b *BTCNode) RemoveIndex(addr string) error {
	err := b.db.Delete([]byte("index_"+addr), nil)
	if err != nil {
		fmt.Printf("Error: %s", err)
		return err
	}
	return nil
}

func (b *BTCNode) StoreUpdate(addr string, update int) error {
	str := strconv.Itoa(update)
	err := b.db.Put([]byte("update_"+addr), []byte(str), nil)
	if err != nil {
		fmt.Printf("Error: %s", err)
		return err
	}
	return nil
}

func (b *BTCNode) RemoveUpdate(addr string) error {
	err := b.db.Delete([]byte("update_"+addr), nil)
	if err != nil {
		fmt.Printf("Error: %s", err)
		return err
	}
	return nil
}

func (b *BTCNode) StoreSpent(key string) error {
	s, err := json.Marshal(true)
	if err != nil {
		fmt.Printf("Error: %s", err)
		return err
	}
	err = b.db.Put([]byte("spent_"+key), []byte(s), nil)
	if err != nil {
		fmt.Printf("Error: %s", err)
		return err
	}
	return nil
}

func (b *BTCNode) StoreTxs(block Block, txs []*Tx) {
	for _, tx := range txs {
		tx.Confirms = block.Height
		tx.Time = block.Time
		tx.Mediantime = block.Mediantime
		go b.StpreTx(tx.Txid, tx)
	}
}

func (b *BTCNode) StpreTx(key string, tx *Tx) error {
	t, err := json.Marshal(tx)
	if err != nil {
		fmt.Printf("Error: %s", err)
		return err
	}
	err = b.db.Put([]byte("txs_"+key), []byte(t), nil)
	if err != nil {
		fmt.Printf("Error: %s", err)
		return err
	}
	return nil
}

func (b *BTCNode) LoadData() error {
	iter := b.db.NewIterator(nil, nil)
	log.Info("loading leveldb....")
	for iter.Next() {
		key := iter.Key()
		value := iter.Value()
		if string(key[:5]) == "index" {
			b.Index[string(key[6:])] = string(value)
		}
		if string(key[:5]) == "spent" {
			b.Spent[string(key[6:])] = true
		}
		if string(key[:6]) == "update" {
			i, err := strconv.Atoi(string(value))
			if err != nil {
				log.Info(err)
				continue
			}
			b.Updates[string(key[7:])] = int64(i)
		}
	}
	iter.Release()
	err := iter.Error()
	if err != nil {
		return err
	}
	log.Infof("loaded completed. index -> %d update -> %d spent -> %d", len(b.Index), len(b.Updates), len(b.Spent))
	return nil
}

func (b *BTCNode) GetBTCTxs(w rest.ResponseWriter, r *rest.Request) {
	address := r.PathParam("address")
	sortFlag := r.FormValue("sort")
	pageFlag := r.FormValue("page")
	spentFlag := r.FormValue("type")

	lock.RLock()
	if b.Index[address] == "" {
		lock.RUnlock()
		res500(w, r)
		return
	}
	txIDs := strings.Split(b.Index[address], "_")
	lock.RUnlock()
	txs := b.LoadTxs(txIDs)
	txRes := []Tx{}
	for _, tx := range txs {
		isSpent := false
		for _, vout := range tx.Vout {
			key := tx.Txid + "_" + strconv.Itoa(vout.N)
			if b.Spent[key] == true {
				vout.Spent = true
				isSpent = true
			}
		}
		if spentFlag == "spent" {
			if isSpent {
				txRes = append(txRes, tx)
			} else {
				continue
			}
		} else {
			txRes = append(txRes, tx)
		}
	}

	if sortFlag == "asc" {
		sort.SliceStable(txRes, func(i, j int) bool {
			return txRes[i].Confirms < txRes[j].Confirms
		})
	} else {
		sort.SliceStable(txRes, func(i, j int) bool {
			return txRes[i].Confirms > txRes[j].Confirms
		})
	}
	pageNum, err := strconv.Atoi(pageFlag)
	if err != nil {
		pageNum = 0
	}
	if len(txRes) >= 150 {
		p := pageNum * 150
		limit := p + 150
		if len(txRes) < limit {
			p = 150 * (len(txRes) / 150)
			limit = len(txRes)
			//log.Info(p, " ", limit, " ", len(txRes))
		}
		txRes = txRes[p:limit]
	}
	w.WriteHeader(http.StatusOK)
	w.WriteJson(txRes)
}

func (b *BTCNode) LoadTxs(txIDs []string) []Tx {
	txRes := []Tx{}
	c := make(chan Tx)
	for _, txID := range txIDs {
		go b.LoadTx("txs_"+txID, c)
	}
	for i := 0; i < len(txIDs); i++ {
		tx := <-c
		txRes = append(txRes, tx)
	}
	return txRes
}

func (b *BTCNode) LoadTx(key string, c chan Tx) {
	tx := Tx{}
	value, err := b.db.Get([]byte(key), nil)
	if err != nil {
		c <- tx
		return
	}
	json.Unmarshal(value, &tx)
	c <- tx
}

func res500(w rest.ResponseWriter, r *rest.Request) {
	w.WriteHeader(http.StatusInternalServerError)
	res := []string{}
	w.WriteJson(res)
}
