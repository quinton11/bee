// Copyright 2020 The Swarm Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package chequebook

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethersphere/bee/pkg/storage"
	"github.com/ethersphere/sw3-bindings/v2/simpleswapfactory"
)

// SendChequeFunc is a function to send cheques.
type SendChequeFunc func(cheque *SignedCheque) error

const (
	lastIssuedChequeKeyPrefix = "chequebook_last_issued_cheque_"
	totalIssuedKey            = "chequebook_total_issued_"
)

var (
	// ErrOutOfFunds is the error when the chequebook has not enough free funds for a cheque
	ErrOutOfFunds = errors.New("chequebook out of funds")
	// ErrInsufficientFunds is the error when the chequebook has not enough free funds for a user action
	ErrInsufficientFunds = errors.New("insufficient token balance")
)

// Service is the main interface for interacting with the nodes chequebook.
type Service interface {
	// Deposit starts depositing erc20 token into the chequebook. This returns once the transactions has been broadcast.
	Deposit(ctx context.Context, amount *big.Int) (hash common.Hash, err error)
	// Withdraw starts withdrawing erc20 token from the chequebook. This returns once the transactions has been broadcast.
	Withdraw(ctx context.Context, amount *big.Int) (hash common.Hash, err error)
	// WaitForDeposit waits for the deposit transaction to confirm and verifies the result.
	WaitForDeposit(ctx context.Context, txHash common.Hash) error
	// Balance returns the token balance of the chequebook.
	Balance(ctx context.Context) (*big.Int, error)
	// AvailableBalance returns the token balance of the chequebook which is not yet used for uncashed cheques.
	AvailableBalance(ctx context.Context) (*big.Int, error)
	// Address returns the address of the used chequebook contract.
	Address() common.Address
	// Issue a new cheque for the beneficiary with an cumulativePayout amount higher than the last.
	Issue(ctx context.Context, beneficiary common.Address, amount *big.Int, sendChequeFunc SendChequeFunc) error
	// LastCheque returns the last cheque we issued for the beneficiary.
	LastCheque(beneficiary common.Address) (*SignedCheque, error)
	// LastCheque returns the last cheques for all beneficiaries.
	LastCheques() (map[common.Address]*SignedCheque, error)
}

type service struct {
	lock               sync.Mutex
	backend            Backend
	transactionService TransactionService

	address            common.Address
	chequebookABI      abi.ABI
	chequebookInstance SimpleSwapBinding
	ownerAddress       common.Address

	erc20Address  common.Address
	erc20ABI      abi.ABI
	erc20Instance ERC20Binding

	store        storage.StateStorer
	chequeSigner ChequeSigner
}

// New creates a new chequebook service for the provided chequebook contract.
func New(backend Backend, transactionService TransactionService, address, erc20Address, ownerAddress common.Address, store storage.StateStorer, chequeSigner ChequeSigner, simpleSwapBindingFunc SimpleSwapBindingFunc, erc20BindingFunc ERC20BindingFunc) (Service, error) {
	chequebookABI, err := abi.JSON(strings.NewReader(simpleswapfactory.ERC20SimpleSwapABI))
	if err != nil {
		return nil, err
	}

	erc20ABI, err := abi.JSON(strings.NewReader(simpleswapfactory.ERC20ABI))
	if err != nil {
		return nil, err
	}

	chequebookInstance, err := simpleSwapBindingFunc(address, backend)
	if err != nil {
		return nil, err
	}

	erc20Instance, err := erc20BindingFunc(erc20Address, backend)
	if err != nil {
		return nil, err
	}

	return &service{
		backend:            backend,
		transactionService: transactionService,
		address:            address,
		chequebookABI:      chequebookABI,
		chequebookInstance: chequebookInstance,
		ownerAddress:       ownerAddress,
		erc20Address:       erc20Address,
		erc20ABI:           erc20ABI,
		erc20Instance:      erc20Instance,
		store:              store,
		chequeSigner:       chequeSigner,
	}, nil
}

// Address returns the address of the used chequebook contract.
func (s *service) Address() common.Address {
	return s.address
}

// Deposit starts depositing erc20 token into the chequebook. This returns once the transactions has been broadcast.
func (s *service) Deposit(ctx context.Context, amount *big.Int) (hash common.Hash, err error) {
	balance, err := s.erc20Instance.BalanceOf(&bind.CallOpts{
		Context: ctx,
	}, s.ownerAddress)
	if err != nil {
		return common.Hash{}, err
	}

	// check we can afford this so we don't waste gas
	if balance.Cmp(amount) < 0 {
		return common.Hash{}, ErrInsufficientFunds
	}

	callData, err := s.erc20ABI.Pack("transfer", s.address, amount)
	if err != nil {
		return common.Hash{}, err
	}

	request := &TxRequest{
		To:       s.erc20Address,
		Data:     callData,
		GasPrice: nil,
		GasLimit: 0,
		Value:    big.NewInt(0),
	}

	txHash, err := s.transactionService.Send(ctx, request)
	if err != nil {
		return common.Hash{}, err
	}

	return txHash, nil
}

// Balance returns the token balance of the chequebook.
func (s *service) Balance(ctx context.Context) (*big.Int, error) {
	return s.chequebookInstance.Balance(&bind.CallOpts{
		Context: ctx,
	})
}

// AvailableBalance returns the token balance of the chequebook which is not yet used for uncashed cheques.
func (s *service) AvailableBalance(ctx context.Context) (*big.Int, error) {
	totalIssued, err := s.totalIssued()
	if err != nil {
		return nil, err
	}

	balance, err := s.Balance(ctx)
	if err != nil {
		return nil, err
	}

	totalPaidOut, err := s.chequebookInstance.TotalPaidOut(&bind.CallOpts{
		Context: ctx,
	})
	if err != nil {
		return nil, err
	}

	// balance plus totalPaidOut is the total amount ever put into the chequebook (ignoring deposits and withdrawals which cancelled out)
	// minus the total amount we issued from this chequebook this gives use the portion of the balance not covered by any cheques
	availableBalance := big.NewInt(0).Add(balance, totalPaidOut)
	availableBalance = availableBalance.Sub(availableBalance, totalIssued)
	return availableBalance, nil
}

// WaitForDeposit waits for the deposit transaction to confirm and verifies the result.
func (s *service) WaitForDeposit(ctx context.Context, txHash common.Hash) error {
	receipt, err := s.transactionService.WaitForReceipt(ctx, txHash)
	if err != nil {
		return err
	}
	if receipt.Status != 1 {
		return ErrTransactionReverted
	}
	return nil
}

// lastIssuedChequeKey computes the key where to store the last cheque for a beneficiary.
func lastIssuedChequeKey(beneficiary common.Address) string {
	return fmt.Sprintf("chequebook_last_issued_cheque_%x", beneficiary)
}

// Issue issues a new cheque and passes it to sendChequeFunc
// if sendChequeFunc succeeds the cheque is considered sent and saved
func (s *service) Issue(ctx context.Context, beneficiary common.Address, amount *big.Int, sendChequeFunc SendChequeFunc) error {
	// don't allow concurrent issuing of cheques
	s.lock.Lock()
	defer s.lock.Unlock()

	availableBalance, err := s.AvailableBalance(ctx)
	if err != nil {
		return err
	}

	if amount.Cmp(availableBalance) > 0 {
		return ErrOutOfFunds
	}

	var cumulativePayout *big.Int
	lastCheque, err := s.LastCheque(beneficiary)
	if err != nil {
		if err != ErrNoCheque {
			return err
		}
		cumulativePayout = big.NewInt(0)
	} else {
		cumulativePayout = lastCheque.CumulativePayout
	}

	// increase cumulativePayout by amount
	cumulativePayout = cumulativePayout.Add(cumulativePayout, amount)

	// create and sign the new cheque
	cheque := Cheque{
		Chequebook:       s.address,
		CumulativePayout: cumulativePayout,
		Beneficiary:      beneficiary,
	}

	sig, err := s.chequeSigner.Sign(&Cheque{
		Chequebook:       s.address,
		CumulativePayout: cumulativePayout,
		Beneficiary:      beneficiary,
	})
	if err != nil {
		return err
	}

	// actually send the check before saving to avoid double payment
	err = sendChequeFunc(&SignedCheque{
		Cheque:    cheque,
		Signature: sig,
	})
	if err != nil {
		return err
	}

	err = s.store.Put(lastIssuedChequeKey(beneficiary), cheque)
	if err != nil {
		return err
	}

	totalIssued, err := s.totalIssued()
	if err != nil {
		return err
	}
	totalIssued = totalIssued.Add(totalIssued, amount)
	return s.store.Put(totalIssuedKey, totalIssued)
}

// returns the total amount in cheques issued so far
func (s *service) totalIssued() (totalIssued *big.Int, err error) {
	err = s.store.Get(totalIssuedKey, &totalIssued)
	if err != nil {
		if err != storage.ErrNotFound {
			return nil, err
		}
		return big.NewInt(0), nil
	}
	return totalIssued, nil
}

// LastCheque returns the last cheque we issued for the beneficiary.
func (s *service) LastCheque(beneficiary common.Address) (*SignedCheque, error) {
	var lastCheque *SignedCheque
	err := s.store.Get(lastIssuedChequeKey(beneficiary), &lastCheque)
	if err != nil {
		if err != storage.ErrNotFound {
			return nil, err
		}
		return nil, ErrNoCheque
	}
	return lastCheque, nil
}

func keyBeneficiary(key []byte, prefix string) (beneficiary common.Address, err error) {
	k := string(key)

	split := strings.SplitAfter(k, prefix)
	if len(split) != 2 {
		return common.Address{}, errors.New("no beneficiary in key")
	}
	return common.HexToAddress(split[1]), nil
}

// LastCheque returns the last cheques for all beneficiaries.
func (s *service) LastCheques() (map[common.Address]*SignedCheque, error) {
	result := make(map[common.Address]*SignedCheque)
	err := s.store.Iterate(lastIssuedChequeKeyPrefix, func(key, val []byte) (stop bool, err error) {
		addr, err := keyBeneficiary(key, lastIssuedChequeKeyPrefix)
		if err != nil {
			return false, fmt.Errorf("parse address from key: %s: %w", string(key), err)
		}

		if _, ok := result[addr]; !ok {

			lastCheque, err := s.LastCheque(addr)
			if err != nil {
				return false, err
			}

			result[addr] = lastCheque
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *service) Withdraw(ctx context.Context, amount *big.Int) (hash common.Hash, err error) {
	availableBalance, err := s.AvailableBalance(ctx)
	if err != nil {
		return common.Hash{}, err
	}

	// check we can afford this so we don't waste gas and don't risk bouncing cheques
	if availableBalance.Cmp(amount) < 0 {
		return common.Hash{}, ErrInsufficientFunds
	}

	callData, err := s.chequebookABI.Pack("withdraw", amount)
	if err != nil {
		return common.Hash{}, err
	}

	request := &TxRequest{
		To:       s.address,
		Data:     callData,
		GasPrice: nil,
		GasLimit: 0,
		Value:    big.NewInt(0),
	}

	txHash, err := s.transactionService.Send(ctx, request)
	if err != nil {
		return common.Hash{}, err
	}

	return txHash, nil
}