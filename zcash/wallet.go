package zcash

import (
	"bytes"
	"fmt"
	"github.com/OpenBazaar/multiwallet/client"
	"github.com/OpenBazaar/multiwallet/config"
	"github.com/OpenBazaar/multiwallet/keys"
	"github.com/OpenBazaar/multiwallet/service"
	"github.com/OpenBazaar/multiwallet/util"
	zaddr "github.com/OpenBazaar/multiwallet/zcash/address"
	wi "github.com/OpenBazaar/wallet-interface"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	hd "github.com/btcsuite/btcutil/hdkeychain"
	"github.com/btcsuite/btcwallet/wallet/txrules"
	bcw "github.com/cpacia/BitcoinCash-Wallet"
	er "github.com/cpacia/BitcoinCash-Wallet/exchangerates"
	"github.com/tyler-smith/go-bip39"
	"golang.org/x/net/proxy"
	"io"
	"time"
)

type ZCashWallet struct {
	db     wi.Datastore
	km     *keys.KeyManager
	params *chaincfg.Params
	client client.APIClient
	ws     *service.WalletService
	fp     *bcw.FeeProvider

	mPrivKey *hd.ExtendedKey
	mPubKey  *hd.ExtendedKey
}

func NewZCashWallet(cfg config.CoinConfig, mnemonic string, params *chaincfg.Params, proxy proxy.Dialer) (*ZCashWallet, error) {
	seed := bip39.NewSeed(mnemonic, "")

	mPrivKey, err := hd.NewMaster(seed, params)
	if err != nil {
		return nil, err
	}
	mPubKey, err := mPrivKey.Neuter()
	if err != nil {
		return nil, err
	}
	km, err := keys.NewKeyManager(cfg.DB.Keys(), params, mPrivKey, wi.Zcash, zcashCashAddress)
	if err != nil {
		return nil, err
	}

	c, err := client.NewInsightClient(cfg.ClientAPI.String(), proxy)
	if err != nil {
		return nil, err
	}

	wm := service.NewWalletService(cfg.DB, km, c, params, wi.Zcash)

	// TODO: create zcash fee provider
	fp := bcw.NewFeeProvider(cfg.MaxFee, cfg.HighFee, cfg.MediumFee, cfg.LowFee, er.NewBitcoinCashPriceFetcher(proxy))

	return &ZCashWallet{cfg.DB, km, params, c, wm, fp, mPrivKey, mPubKey}, nil
}

func zcashCashAddress(key *hd.ExtendedKey, params *chaincfg.Params) (btcutil.Address, error) {
	addr, err := key.Address(params)
	if err != nil {
		return nil, err
	}
	return zaddr.NewAddressPubKeyHash(addr.ScriptAddress(), params)
}

func (w *ZCashWallet) Start() {
	w.ws.Start()
}

func (w *ZCashWallet) Params() *chaincfg.Params {
	return w.params
}

func (w *ZCashWallet) CurrencyCode() string {
	if w.params.Name == chaincfg.MainNetParams.Name {
		return "zec"
	} else {
		return "tzec"
	}
}

func (w *ZCashWallet) IsDust(amount int64) bool {
	return txrules.IsDustAmount(btcutil.Amount(amount), 25, txrules.DefaultRelayFeePerKb)
}

func (w *ZCashWallet) MasterPrivateKey() *hd.ExtendedKey {
	return w.mPrivKey
}

func (w *ZCashWallet) MasterPublicKey() *hd.ExtendedKey {
	return w.mPubKey
}

func (w *ZCashWallet) CurrentAddress(purpose wi.KeyPurpose) btcutil.Address {
	key, _ := w.km.GetCurrentKey(purpose)
	addr, _ := zcashCashAddress(key, w.params)
	return btcutil.Address(addr)
}

func (w *ZCashWallet) NewAddress(purpose wi.KeyPurpose) btcutil.Address {
	i, _ := w.db.Keys().GetUnused(purpose)
	key, _ := w.km.GenerateChildKey(purpose, uint32(i[1]))
	addr, _ := zcashCashAddress(key, w.params)
	w.db.Keys().MarkKeyAsUsed(addr.ScriptAddress())
	return btcutil.Address(addr)
}

func (w *ZCashWallet) DecodeAddress(addr string) (btcutil.Address, error) {
	return zaddr.DecodeAddress(addr, w.params)
}

func (w *ZCashWallet) ScriptToAddress(script []byte) (btcutil.Address, error) {
	addr, err := zaddr.ExtractPkScriptAddrs(script, w.params)
	if err != nil {
		return nil, err
	}
	return addr, nil
}

func (w *ZCashWallet) AddressToScript(addr btcutil.Address) ([]byte, error) {
	return zaddr.PayToAddrScript(addr)
}

func (w *ZCashWallet) HasKey(addr btcutil.Address) bool {
	_, err := w.km.GetKeyForScript(addr.ScriptAddress())
	if err != nil {
		return false
	}
	return true
}

func (w *ZCashWallet) Balance() (confirmed, unconfirmed int64) {
	utxos, _ := w.db.Utxos().GetAll()
	txns, _ := w.db.Txns().GetAll(false)
	return util.CalcBalance(utxos, txns)
}

func (w *ZCashWallet) Transactions() ([]wi.Txn, error) {
	return w.db.Txns().GetAll(false)
}

func (w *ZCashWallet) GetTransaction(txid chainhash.Hash) (wi.Txn, error) {
	txn, err := w.db.Txns().Get(txid)
	return txn, err
}

func (w *ZCashWallet) ChainTip() (uint32, chainhash.Hash) {
	return w.ws.ChainTip()
}

func (w *ZCashWallet) GetFeePerByte(feeLevel wi.FeeLevel) uint64 {
	return w.fp.GetFeePerByte(feeLevel)
}

func (w *ZCashWallet) Spend(amount int64, addr btcutil.Address, feeLevel wi.FeeLevel) (*chainhash.Hash, error) {
	tx, err := w.buildTx(amount, addr, feeLevel, nil)
	if err != nil {
		return nil, err
	}
	// Broadcast
	var buf bytes.Buffer
	tx.BtcEncode(&buf, wire.ProtocolVersion, wire.WitnessEncoding)

	_, err = w.client.Broadcast(buf.Bytes())
	if err != nil {
		return nil, err
	}

	ch := tx.TxHash()
	return &ch, nil
}

func (w *ZCashWallet) BumpFee(txid chainhash.Hash) (*chainhash.Hash, error) {
	return w.bumpFee(txid)
}

func (w *ZCashWallet) EstimateFee(ins []wi.TransactionInput, outs []wi.TransactionOutput, feePerByte uint64) uint64 {
	tx := new(wire.MsgTx)
	for _, out := range outs {
		output := wire.NewTxOut(out.Value, out.ScriptPubKey)
		tx.TxOut = append(tx.TxOut, output)
	}
	estimatedSize := EstimateSerializeSize(len(ins), tx.TxOut, false, P2PKH)
	fee := estimatedSize * int(feePerByte)
	return uint64(fee)
}

func (w *ZCashWallet) EstimateSpendFee(amount int64, feeLevel wi.FeeLevel) (uint64, error) {
	return w.estimateSpendFee(amount, feeLevel)
}

func (w *ZCashWallet) SweepAddress(utxos []wi.Utxo, address *btcutil.Address, key *hd.ExtendedKey, redeemScript *[]byte, feeLevel wi.FeeLevel) (*chainhash.Hash, error) {
	return w.sweepAddress(utxos, address, key, redeemScript, feeLevel)
}

func (w *ZCashWallet) CreateMultisigSignature(ins []wi.TransactionInput, outs []wi.TransactionOutput, key *hd.ExtendedKey, redeemScript []byte, feePerByte uint64) ([]wi.Signature, error) {
	return w.createMultisigSignature(ins, outs, key, redeemScript, feePerByte)
}

func (w *ZCashWallet) Multisign(ins []wi.TransactionInput, outs []wi.TransactionOutput, sigs1 []wi.Signature, sigs2 []wi.Signature, redeemScript []byte, feePerByte uint64, broadcast bool) ([]byte, error) {
	return w.multisign(ins, outs, sigs1, sigs2, redeemScript, feePerByte, broadcast)
}

func (w *ZCashWallet) GenerateMultisigScript(keys []hd.ExtendedKey, threshold int, timeout time.Duration, timeoutKey *hd.ExtendedKey) (addr btcutil.Address, redeemScript []byte, err error) {
	return w.generateMultisigScript(keys, threshold, timeout, timeoutKey)
}

func (w *ZCashWallet) AddWatchedScript(script []byte) error {
	err := w.db.WatchedScripts().Put(script)
	if err != nil {
		return err
	}
	addr, err := w.ScriptToAddress(script)
	if err != nil {
		return err
	}
	w.client.ListenAddress(addr)
	return nil
}

func (w *ZCashWallet) AddTransactionListener(callback func(wi.TransactionCallback)) {
	w.ws.AddTransactionListener(callback)
}

func (w *ZCashWallet) ReSyncBlockchain(fromTime time.Time) {
	go w.ws.UpdateState()
}

func (w *ZCashWallet) GetConfirmations(txid chainhash.Hash) (uint32, uint32, error) {
	txn, err := w.db.Txns().Get(txid)
	if err != nil {
		return 0, 0, err
	}
	if txn.Height == 0 {
		return 0, 0, nil
	}
	chainTip, _ := w.ChainTip()
	return chainTip - uint32(txn.Height) + 1, uint32(txn.Height), nil
}

func (w *ZCashWallet) Close() {
	w.ws.Stop()
	w.client.Close()
}

func (w *ZCashWallet) DumpTables(wr io.Writer) {
	fmt.Fprintln(wr, "Transactions-----")
	txns, _ := w.db.Txns().GetAll(true)
	for _, tx := range txns {
		fmt.Fprintf(wr, "Hash: %s, Height: %d, Value: %d, WatchOnly: %t\n", tx.Txid, int(tx.Height), int(tx.Value), tx.WatchOnly)
	}
	fmt.Fprintln(wr, "\nUtxos-----")
	utxos, _ := w.db.Utxos().GetAll()
	for _, u := range utxos {
		fmt.Fprintf(wr, "Hash: %s, Index: %d, Height: %d, Value: %d, WatchOnly: %t\n", u.Op.Hash.String(), int(u.Op.Index), int(u.AtHeight), int(u.Value), u.WatchOnly)
	}
}
