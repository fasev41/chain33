package types

import (
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"

	ctypes "github.com/33cn/chain33/types"
	"github.com/ethereum/go-ethereum/common/hexutil"
	etypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
)

//makeDERSigToRSV der sig data to rsv
func MakeDERSigToRSV(eipSigner etypes.EIP155Signer, sig []byte) (r, s, v *big.Int, err error) {
	//fmt.Println("len:",len(sig),"sig",hexutil.Encode(sig))
	if len(sig) == 65 {
		r = new(big.Int).SetBytes(sig[:32])
		s = new(big.Int).SetBytes(sig[32:64])
		v = new(big.Int).SetBytes([]byte{sig[64]})
		return
	}

	rb, sb, err := paraseDERCode(sig)
	if err != nil {
		fmt.Println("makeDERSigToRSV", "err", err.Error(), "sig", hexutil.Encode(sig))
		return nil, nil, nil, err
	}
	var signature []byte
	signature = append(signature, rb...)
	signature = append(signature, sb...)
	signature = append(signature, 0x00)
	r, s, v = decodeSignature(signature)
	if eipSigner.ChainID().Sign() != 0 {
		v = big.NewInt(int64(signature[64] + 35))
		v.Add(v, new(big.Int).Mul(eipSigner.ChainID(), big.NewInt(2)))
	}
	return r, s, v, nil
}

func decodeSignature(sig []byte) (r, s, v *big.Int) {
	if len(sig) != crypto.SignatureLength {
		panic(fmt.Sprintf("wrong size for signature: got %d, want %d", len(sig), crypto.SignatureLength))
	}
	r = new(big.Int).SetBytes(sig[:32])
	s = new(big.Int).SetBytes(sig[32:64])
	v = new(big.Int).SetBytes([]byte{sig[64] + 27})
	return r, s, v
}

func paraseDERCode(sig []byte) (r, s []byte, err error) {
	//0x30 [total-length] 0x02 [R-length] [R] 0x02 [S-length] [S]

	if len(sig) < 70 || len(sig) > 71 {
		return nil, nil, fmt.Errorf("wrong sig data size:%v,must beyound length 70 bytes", len(sig))
	}
	//3045022100af5778b81ae8817c6ae29fad8c1113d501e521c885a65c2c4d71763c4963984b022020687b73f5c90243dc16c99427d6593a711c52c8bf09ca6331cdd42c66edee74
	if sig[0] == 0x30 && sig[2] == 0x02 {
		r = sig[4 : int(sig[3])+4]
		if r[0] == 0x0 {
			r = r[1:]
		}
	}
	if sig[int(sig[3])+4] == 0x02 { //&&sig[int(sig[3])+5]==0x20
		s = sig[int(sig[3])+6 : int(sig[3])+6+int(sig[int(sig[3])+5])]
	}

	return
}

func MakeDERsignature(rb, sb []byte) []byte {
	if rb[0] > 0x7F {
		rb = append([]byte{0}, rb...)
	}

	if sb[0] > 0x7F {
		sb = append([]byte{0}, sb...)
	}
	// total length of returned signature is 1 byte for each magic and
	// length (6 total), plus lengths of r and s
	length := 6 + len(rb) + len(sb)
	b := make([]byte, length)

	b[0] = 0x30
	b[1] = byte(length - 2)
	b[2] = 0x02
	b[3] = byte(len(rb))
	offset := copy(b[4:], rb) + 4
	b[offset] = 0x02
	b[offset+1] = byte(len(sb))
	copy(b[offset+2:], sb)
	return b
}

//TxsToEthTxs chain33 txs format transfer to eth txs format
func TxsToEthTxs(ctxs []*ctypes.Transaction, cfg *ctypes.Chain33Config, full bool) (txs []interface{}, err error) {
	eipSigner := etypes.NewEIP155Signer(big.NewInt(int64(cfg.GetChainID())))
	for _, itx := range ctxs {
		var tx Transaction
		tx.Hash = common.BytesToHash(itx.Hash())
		if !full {
			txs = append(txs, tx.Hash)
			continue
		}

		tx.Type = etypes.LegacyTxType
		from := common.HexToAddress(itx.From())
		to := common.HexToAddress(itx.To)
		tx.From = &from
		tx.To = &to
		amount, err := itx.Amount()
		if err != nil {
			log.Error("TxsToEthTxs", "err", err)
			return nil, err
		}
		tx.Value = (*hexutil.Big)(big.NewInt(amount))

		if strings.HasSuffix(string(itx.Execer), "evm") {
			var action ctypes.EVMContractAction4Chain33
			err := ctypes.Decode(itx.GetPayload(), &action)
			if err != nil {
				log.Error("TxDetailsToEthTx", "err", err)
				continue
			}
			data := (hexutil.Bytes)(action.Para)
			tx.Data = &data
			//如果是EVM交易，to为合约交易
			caddr := common.HexToAddress(action.ContractAddr)
			tx.To = &caddr
		}
		r, s, v, err := MakeDERSigToRSV(eipSigner, itx.Signature.GetSignature())
		if err != nil {
			log.Error("makeDERSigToRSV", "err", err)
			txs = append(txs, &tx)
			continue
		}
		tx.V = (*hexutil.Big)(v)
		tx.R = (*hexutil.Big)(r)
		tx.S = (*hexutil.Big)(s)
		txs = append(txs, &tx)
	}
	return txs, nil
}

//TxDetailsToEthTx chain33 txdetails transfer to eth tx
func TxDetailsToEthTx(txdetails *ctypes.TransactionDetails, cfg *ctypes.Chain33Config) (txs Transactions, receipts []*Receipt, err error) {
	for _, detail := range txdetails.GetTxs() {
		var tx Transaction
		var receipt Receipt
		//处理 execer=EVM
		to := common.HexToAddress(detail.Tx.To)
		tx.To = &to
		//receipt.To = tx.To
		if strings.HasSuffix(string(detail.Tx.Execer), "evm") {
			var action ctypes.EVMContractAction4Chain33
			err := ctypes.Decode(detail.GetTx().GetPayload(), &action)
			if err != nil {
				log.Error("TxDetailsToEthTx", "err", err)
				continue
			}
			var decodeData []byte
			if len(action.Para) != 0 {
				decodeData = action.Para
			} else {
				decodeData = action.Code
			}
			data := (hexutil.Bytes)(decodeData)
			tx.Data = &data
			//如果是EVM交易，to为合约交易
			caddr := common.HexToAddress(action.ContractAddr)
			tx.To = &caddr
			receipt.ContractAddress = caddr
		}
		from := common.HexToAddress(detail.Fromaddr)
		tx.From = &from
		tx.Value = (*hexutil.Big)(big.NewInt(detail.GetAmount())) //fmt.Sprintf("0x%x",detail.GetAmount())//.
		tx.Type = 0
		tx.BlockNumber = hexutil.Big(*big.NewInt(detail.GetHeight())) //fmt.Sprintf("0x%x",detail.Height)
		//tx.TransactionIndex = hexutil.Uint(uint(detail.GetIndex()))   //fmt.Sprintf("0x%x",detail.GetIndex())
		eipSigner := etypes.NewEIP155Signer(big.NewInt(int64(cfg.GetChainID())))
		r, s, v, err := MakeDERSigToRSV(eipSigner, detail.Tx.GetSignature().GetSignature())
		if err != nil {
			return nil, nil, err
		}
		tx.V = (*hexutil.Big)(v)
		tx.R = (*hexutil.Big)(r)
		tx.S = (*hexutil.Big)(s)
		tx.Hash = common.BytesToHash(detail.Tx.Hash())

		txs = append(txs, &tx)
		//receipt.To = detail.Tx.To
		//receipt.From = detail.Fromaddr
		if detail.Receipt.Ty == 2 { //success
			receipt.Status = 1
		} else {
			receipt.Status = 0
		}
		//jbs, _ := json.MarshalIndent(detail.Receipt.Logs, " ", "\t")
		//fmt.Println("detail.Receipt.Logs", string(jbs))

		receipt.Logs = receiptLogs2EvmLog(detail.Receipt.Logs, nil)
		receipt.Bloom = CreateBloom([]*Receipt{&receipt})
		receipt.TxHash = common.BytesToHash(detail.GetTx().Hash())
		receipt.BlockNumber = (*hexutil.Big)(big.NewInt(detail.Height))
		receipt.TransactionIndex = hexutil.Uint(uint64(detail.GetIndex()))
		receipts = append(receipts, &receipt)
	}
	return
}

func receiptLogs2EvmLog(logs []*ctypes.ReceiptLog, option *SubLogs) (elogs []*EvmLog) {
	var filterTopics = make(map[string]bool)
	if option != nil {
		for _, topic := range option.Topics {
			filterTopics[topic] = true
		}
	}
	for _, lg := range logs {
		if lg.Ty != 605 { //evm event
			continue
		}

		var evmLog ctypes.EVMLog
		err := ctypes.Decode(lg.Log, &evmLog)
		if nil != err {
			continue
		}
		var log EvmLog
		log.Data = evmLog.Data

		for _, topic := range evmLog.Topic {
			if option != nil {
				if _, ok := filterTopics[hexutil.Encode(topic)]; !ok {
					continue
				}
			}

			log.Topics = append(log.Topics, common.BytesToHash(topic))
		}
		elogs = append(elogs, &log)
	}
	return
}

//FilterEvmLogs filter evm logs by option
func FilterEvmLogs(logs *ctypes.EVMTxLogPerBlk, option *SubLogs) (evmlogs []*EvmLogInfo) {
	var address string
	var filterTopics = make(map[string]bool)
	if option != nil {
		for _, topic := range option.Topics {
			if topic == "" {
				continue
			}
			filterTopics[topic] = true
		}
	}

	if option != nil {
		address = option.Address
	}
	for i, txlog := range logs.TxAndLogs {
		var info EvmLogInfo
		if txlog.GetTx().GetTo() == address {
			for j, tlog := range txlog.GetLogsPerTx().GetLogs() {
				var topics []string
				if _, ok := filterTopics[hexutil.Encode(tlog.Topic[0])]; ok || len(filterTopics) == 0 {
					topics = append(topics, hexutil.Encode(tlog.Topic[0]))
				}

				if len(topics) != 0 {
					info.LogIndex = hexutil.EncodeUint64(uint64(j))
					info.Topics = topics
				}
				info.Address = address
				info.TransactionIndex = hexutil.EncodeUint64(uint64(i))
				info.BlockHash = hexutil.Encode(logs.BlockHash)
				info.TransactionHash = hexutil.Encode(txlog.GetTx().Hash())
				info.BlockNumber = hexutil.EncodeUint64(uint64(logs.Height))
				evmlogs = append(evmlogs, &info)
			}
		}
	}

	return
}

func ParaseEthSigData(sig []byte, chainID int32) ([]byte, error) {
	if len(sig) != 65 {
		return nil, errors.New("invalid pub key byte,must be 65 bytes")
	}

	if sig[64] != 0 && sig[64] != 1 {
		if sig[64] == 27 || sig[64] == 28 {
			sig[64] = sig[64] - 27
			return sig, nil

		} else {
			//salt := 1
			//if chainID == 0 {
			//	salt = 0
			//}
			chainIDMul := 2 * chainID
			sig[64] = sig[64] - byte(chainIDMul) - 35
			return sig, nil
		}

	}

	return sig, nil

}

// CreateBloom creates a bloom filter out of the give Receipts (+Logs)
func CreateBloom(receipts []*Receipt) etypes.Bloom {
	var bin etypes.Bloom
	for _, receipt := range receipts {
		for _, log := range receipt.Logs {
			if log.Address.Bytes() != nil {
				bin.Add(log.Address.Bytes())
			}

			for _, b := range log.Topics {
				bin.Add(b[:])
			}
		}
	}
	return bin
}
