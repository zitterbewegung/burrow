// Copyright 2017 Monax Industries Limited
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package vm

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"

	"github.com/hyperledger/burrow/common/sanity"
	. "github.com/hyperledger/burrow/manager/burrow-mint/evm/opcodes"
	"github.com/hyperledger/burrow/manager/burrow-mint/evm/sha3"
	ptypes "github.com/hyperledger/burrow/permission/types"
	"github.com/hyperledger/burrow/txs"
	. "github.com/hyperledger/burrow/word256"

	"github.com/tendermint/go-events"
)

var (
	ErrUnknownAddress         = errors.New("Unknown address")
	ErrInsufficientBalance    = errors.New("Insufficient balance")
	ErrInvalidJumpDest        = errors.New("Invalid jump dest")
	ErrInsufficientGas        = errors.New("Insufficient gas")
	ErrMemoryOutOfBounds      = errors.New("Memory out of bounds")
	ErrCodeOutOfBounds        = errors.New("Code out of bounds")
	ErrInputOutOfBounds       = errors.New("Input out of bounds")
	ErrCallStackOverflow      = errors.New("Call stack overflow")
	ErrCallStackUnderflow     = errors.New("Call stack underflow")
	ErrDataStackOverflow      = errors.New("Data stack overflow")
	ErrDataStackUnderflow     = errors.New("Data stack underflow")
	ErrInvalidContract        = errors.New("Invalid contract")
	ErrNativeContractCodeCopy = errors.New("Tried to copy native contract code")
)

type ErrPermission struct {
	typ string
}

func (err ErrPermission) Error() string {
	return fmt.Sprintf("Contract does not have permission to %s", err.typ)
}

const (
	dataStackCapacity = 1024
	callStackCapacity = 100 // TODO ensure usage.
)

type Debug bool

var dbg Debug

func SetDebug(d bool) {
	dbg = Debug(d)
}

func (d Debug) Printf(s string, a ...interface{}) {
	if d {
		fmt.Printf(s, a...)
	}
}

type VM struct {
	appState       AppState
	memoryProvider func() Memory
	params         Params
	origin         Word256
	txid           []byte

	callDepth int

	evc events.Fireable
}

func NewVM(appState AppState, memoryProvider func() Memory, params Params,
	origin Word256, txid []byte) *VM {
	return &VM{
		appState:       appState,
		memoryProvider: memoryProvider,
		params:         params,
		origin:         origin,
		callDepth:      0,
		txid:           txid,
	}
}

// satisfies events.Eventable
func (vm *VM) SetFireable(evc events.Fireable) {
	vm.evc = evc
}

// CONTRACT: it is the duty of the contract writer to call known permissions
// we do not convey if a permission is not set
// (unlike in state/execution, where we guarantee HasPermission is called
// on known permissions and panics else)
// If the perm is not defined in the acc nor set by default in GlobalPermissions,
// this function returns false.
func HasPermission(appState AppState, acc *Account, perm ptypes.PermFlag) bool {
	v, err := acc.Permissions.Base.Get(perm)
	if _, ok := err.(ptypes.ErrValueNotSet); ok {
		if appState == nil {
			// In this case the permission is unknown
			return false
		}
		return HasPermission(nil, appState.GetAccount(ptypes.GlobalPermissionsAddress256), perm)
	}
	return v
}

func (vm *VM) fireCallEvent(exception *string, output *[]byte, caller, callee *Account, input []byte, value int64, gas *int64) {
	// fire the post call event (including exception if applicable)
	if vm.evc != nil {
		vm.evc.FireEvent(txs.EventStringAccCall(callee.Address.Postfix(20)), txs.EventDataCall{
			&txs.CallData{caller.Address.Postfix(20), callee.Address.Postfix(20), input, value, *gas},
			vm.origin.Postfix(20),
			vm.txid,
			*output,
			*exception,
		})
	}
}

// CONTRACT appState is aware of caller and callee, so we can just mutate them.
// CONTRACT code and input are not mutated.
// CONTRACT returned 'ret' is a new compact slice.
// value: To be transferred from caller to callee. Refunded upon error.
// gas:   Available gas. No refunds for gas.
// code: May be nil, since the CALL opcode may be used to send value from contracts to accounts
func (vm *VM) Call(caller, callee *Account, code, input []byte, value int64, gas *int64) (output []byte, err error) {

	exception := new(string)
	// fire the post call event (including exception if applicable)
	defer vm.fireCallEvent(exception, &output, caller, callee, input, value, gas)

	if err = transfer(caller, callee, value); err != nil {
		*exception = err.Error()
		return
	}

	if len(code) > 0 {
		vm.callDepth += 1
		output, err = vm.call(caller, callee, code, input, value, gas)
		vm.callDepth -= 1
		if err != nil {
			*exception = err.Error()
			err := transfer(callee, caller, value)
			if err != nil {
				// data has been corrupted in ram
				sanity.PanicCrisis("Could not return value to caller")
			}
		}
	}

	return
}

// DelegateCall is executed by the DELEGATECALL opcode, introduced as off Ethereum Homestead.
// The intent of delegate call is to run the code of the callee in the storage context of the caller;
// while preserving the original caller to the previous callee.
// Different to the normal CALL or CALLCODE, the value does not need to be transferred to the callee.
func (vm *VM) DelegateCall(caller, callee *Account, code, input []byte, value int64, gas *int64) (output []byte, err error) {

	exception := new(string)
	// fire the post call event (including exception if applicable)
	// NOTE: [ben] hotfix for issue 371;
	// introduce event EventStringAccDelegateCall Acc/%X/DelegateCall
	// defer vm.fireCallEvent(exception, &output, caller, callee, input, value, gas)

	// DelegateCall does not transfer the value to the callee.

	if len(code) > 0 {
		vm.callDepth += 1
		output, err = vm.call(caller, callee, code, input, value, gas)
		vm.callDepth -= 1
		if err != nil {
			*exception = err.Error()
		}
	}

	return
}

// Try to deduct gasToUse from gasLeft.  If ok return false, otherwise
// set err and return true.
func useGasNegative(gasLeft *int64, gasToUse int64, err *error) bool {
	if *gasLeft >= gasToUse {
		*gasLeft -= gasToUse
		return false
	} else if *err == nil {
		*err = ErrInsufficientGas
	}
	return true
}

// Just like Call() but does not transfer 'value' or modify the callDepth.
func (vm *VM) call(caller, callee *Account, code, input []byte, value int64, gas *int64) (output []byte, err error) {
	dbg.Printf("(%d) (%X) %X (code=%d) gas: %v (d) %X\n", vm.callDepth, caller.Address[:4], callee.Address, len(callee.Code), *gas, input)

	var (
		pc     int64 = 0
		stack        = NewStack(dataStackCapacity, gas, &err)
		memory       = vm.memoryProvider()
	)

	for {
		// Use BaseOp gas.
		if useGasNegative(gas, GasBaseOp, &err) {
			return nil, err
		}

		var op = codeGetOp(code, pc)
		dbg.Printf("(pc) %-3d (op) %-14s (st) %-4d ", pc, op.String(), stack.Len())

		switch op {

		case ADD: // 0x01
			x, y := stack.Pop(), stack.Pop()
			xb := new(big.Int).SetBytes(x[:])
			yb := new(big.Int).SetBytes(y[:])
			sum := new(big.Int).Add(xb, yb)
			res := LeftPadWord256(U256(sum).Bytes())
			stack.Push(res)
			dbg.Printf(" %v + %v = %v (%X)\n", xb, yb, sum, res)

		case MUL: // 0x02
			x, y := stack.Pop(), stack.Pop()
			xb := new(big.Int).SetBytes(x[:])
			yb := new(big.Int).SetBytes(y[:])
			prod := new(big.Int).Mul(xb, yb)
			res := LeftPadWord256(U256(prod).Bytes())
			stack.Push(res)
			dbg.Printf(" %v * %v = %v (%X)\n", xb, yb, prod, res)

		case SUB: // 0x03
			x, y := stack.Pop(), stack.Pop()
			xb := new(big.Int).SetBytes(x[:])
			yb := new(big.Int).SetBytes(y[:])
			diff := new(big.Int).Sub(xb, yb)
			res := LeftPadWord256(U256(diff).Bytes())
			stack.Push(res)
			dbg.Printf(" %v - %v = %v (%X)\n", xb, yb, diff, res)

		case DIV: // 0x04
			x, y := stack.Pop(), stack.Pop()
			if y.IsZero() {
				stack.Push(Zero256)
				dbg.Printf(" %x / %x = %v\n", x, y, 0)
			} else {
				xb := new(big.Int).SetBytes(x[:])
				yb := new(big.Int).SetBytes(y[:])
				div := new(big.Int).Div(xb, yb)
				res := LeftPadWord256(U256(div).Bytes())
				stack.Push(res)
				dbg.Printf(" %v / %v = %v (%X)\n", xb, yb, div, res)
			}

		case SDIV: // 0x05
			x, y := stack.Pop(), stack.Pop()
			if y.IsZero() {
				stack.Push(Zero256)
				dbg.Printf(" %x / %x = %v\n", x, y, 0)
			} else {
				xb := S256(new(big.Int).SetBytes(x[:]))
				yb := S256(new(big.Int).SetBytes(y[:]))
				div := new(big.Int).Div(xb, yb)
				res := LeftPadWord256(U256(div).Bytes())
				stack.Push(res)
				dbg.Printf(" %v / %v = %v (%X)\n", xb, yb, div, res)
			}

		case MOD: // 0x06
			x, y := stack.Pop(), stack.Pop()
			if y.IsZero() {
				stack.Push(Zero256)
				dbg.Printf(" %v %% %v = %v\n", x, y, 0)
			} else {
				xb := new(big.Int).SetBytes(x[:])
				yb := new(big.Int).SetBytes(y[:])
				mod := new(big.Int).Mod(xb, yb)
				res := LeftPadWord256(U256(mod).Bytes())
				stack.Push(res)
				dbg.Printf(" %v %% %v = %v (%X)\n", xb, yb, mod, res)
			}

		case SMOD: // 0x07
			x, y := stack.Pop(), stack.Pop()
			if y.IsZero() {
				stack.Push(Zero256)
				dbg.Printf(" %v %% %v = %v\n", x, y, 0)
			} else {
				xb := S256(new(big.Int).SetBytes(x[:]))
				yb := S256(new(big.Int).SetBytes(y[:]))
				mod := new(big.Int).Mod(xb, yb)
				res := LeftPadWord256(U256(mod).Bytes())
				stack.Push(res)
				dbg.Printf(" %v %% %v = %v (%X)\n", xb, yb, mod, res)
			}

		case ADDMOD: // 0x08
			x, y, z := stack.Pop(), stack.Pop(), stack.Pop()
			if z.IsZero() {
				stack.Push(Zero256)
				dbg.Printf(" %v %% %v = %v\n", x, y, 0)
			} else {
				xb := new(big.Int).SetBytes(x[:])
				yb := new(big.Int).SetBytes(y[:])
				zb := new(big.Int).SetBytes(z[:])
				add := new(big.Int).Add(xb, yb)
				mod := new(big.Int).Mod(add, zb)
				res := LeftPadWord256(U256(mod).Bytes())
				stack.Push(res)
				dbg.Printf(" %v + %v %% %v = %v (%X)\n",
					xb, yb, zb, mod, res)
			}

		case MULMOD: // 0x09
			x, y, z := stack.Pop(), stack.Pop(), stack.Pop()
			if z.IsZero() {
				stack.Push(Zero256)
				dbg.Printf(" %v %% %v = %v\n", x, y, 0)
			} else {
				xb := new(big.Int).SetBytes(x[:])
				yb := new(big.Int).SetBytes(y[:])
				zb := new(big.Int).SetBytes(z[:])
				mul := new(big.Int).Mul(xb, yb)
				mod := new(big.Int).Mod(mul, zb)
				res := LeftPadWord256(U256(mod).Bytes())
				stack.Push(res)
				dbg.Printf(" %v * %v %% %v = %v (%X)\n",
					xb, yb, zb, mod, res)
			}

		case EXP: // 0x0A
			x, y := stack.Pop(), stack.Pop()
			xb := new(big.Int).SetBytes(x[:])
			yb := new(big.Int).SetBytes(y[:])
			pow := new(big.Int).Exp(xb, yb, big.NewInt(0))
			res := LeftPadWord256(U256(pow).Bytes())
			stack.Push(res)
			dbg.Printf(" %v ** %v = %v (%X)\n", xb, yb, pow, res)

		case SIGNEXTEND: // 0x0B
			back := stack.Pop()
			backb := new(big.Int).SetBytes(back[:])
			if backb.Cmp(big.NewInt(31)) < 0 {
				bit := uint(backb.Uint64()*8 + 7)
				num := stack.Pop()
				numb := new(big.Int).SetBytes(num[:])
				mask := new(big.Int).Lsh(big.NewInt(1), bit)
				mask.Sub(mask, big.NewInt(1))
				if numb.Bit(int(bit)) == 1 {
					numb.Or(numb, mask.Not(mask))
				} else {
					numb.Add(numb, mask)
				}
				res := LeftPadWord256(U256(numb).Bytes())
				dbg.Printf(" = %v (%X)", numb, res)
				stack.Push(res)
			}

		case LT: // 0x10
			x, y := stack.Pop(), stack.Pop()
			xb := new(big.Int).SetBytes(x[:])
			yb := new(big.Int).SetBytes(y[:])
			if xb.Cmp(yb) < 0 {
				stack.Push64(1)
				dbg.Printf(" %v < %v = %v\n", xb, yb, 1)
			} else {
				stack.Push(Zero256)
				dbg.Printf(" %v < %v = %v\n", xb, yb, 0)
			}

		case GT: // 0x11
			x, y := stack.Pop(), stack.Pop()
			xb := new(big.Int).SetBytes(x[:])
			yb := new(big.Int).SetBytes(y[:])
			if xb.Cmp(yb) > 0 {
				stack.Push64(1)
				dbg.Printf(" %v > %v = %v\n", xb, yb, 1)
			} else {
				stack.Push(Zero256)
				dbg.Printf(" %v > %v = %v\n", xb, yb, 0)
			}

		case SLT: // 0x12
			x, y := stack.Pop(), stack.Pop()
			xb := S256(new(big.Int).SetBytes(x[:]))
			yb := S256(new(big.Int).SetBytes(y[:]))
			if xb.Cmp(yb) < 0 {
				stack.Push64(1)
				dbg.Printf(" %v < %v = %v\n", xb, yb, 1)
			} else {
				stack.Push(Zero256)
				dbg.Printf(" %v < %v = %v\n", xb, yb, 0)
			}

		case SGT: // 0x13
			x, y := stack.Pop(), stack.Pop()
			xb := S256(new(big.Int).SetBytes(x[:]))
			yb := S256(new(big.Int).SetBytes(y[:]))
			if xb.Cmp(yb) > 0 {
				stack.Push64(1)
				dbg.Printf(" %v > %v = %v\n", xb, yb, 1)
			} else {
				stack.Push(Zero256)
				dbg.Printf(" %v > %v = %v\n", xb, yb, 0)
			}

		case EQ: // 0x14
			x, y := stack.Pop(), stack.Pop()
			if bytes.Equal(x[:], y[:]) {
				stack.Push64(1)
				dbg.Printf(" %X == %X = %v\n", x, y, 1)
			} else {
				stack.Push(Zero256)
				dbg.Printf(" %X == %X = %v\n", x, y, 0)
			}

		case ISZERO: // 0x15
			x := stack.Pop()
			if x.IsZero() {
				stack.Push64(1)
				dbg.Printf(" %v == 0 = %v\n", x, 1)
			} else {
				stack.Push(Zero256)
				dbg.Printf(" %v == 0 = %v\n", x, 0)
			}

		case AND: // 0x16
			x, y := stack.Pop(), stack.Pop()
			z := [32]byte{}
			for i := 0; i < 32; i++ {
				z[i] = x[i] & y[i]
			}
			stack.Push(z)
			dbg.Printf(" %X & %X = %X\n", x, y, z)

		case OR: // 0x17
			x, y := stack.Pop(), stack.Pop()
			z := [32]byte{}
			for i := 0; i < 32; i++ {
				z[i] = x[i] | y[i]
			}
			stack.Push(z)
			dbg.Printf(" %X | %X = %X\n", x, y, z)

		case XOR: // 0x18
			x, y := stack.Pop(), stack.Pop()
			z := [32]byte{}
			for i := 0; i < 32; i++ {
				z[i] = x[i] ^ y[i]
			}
			stack.Push(z)
			dbg.Printf(" %X ^ %X = %X\n", x, y, z)

		case NOT: // 0x19
			x := stack.Pop()
			z := [32]byte{}
			for i := 0; i < 32; i++ {
				z[i] = ^x[i]
			}
			stack.Push(z)
			dbg.Printf(" !%X = %X\n", x, z)

		case BYTE: // 0x1A
			idx, val := stack.Pop64(), stack.Pop()
			res := byte(0)
			if idx < 32 {
				res = val[idx]
			}
			stack.Push64(int64(res))
			dbg.Printf(" => 0x%X\n", res)

		case SHA3: // 0x20
			if useGasNegative(gas, GasSha3, &err) {
				return nil, err
			}
			offset, size := stack.Pop64(), stack.Pop64()
			data, memErr := memory.Read(offset, size)
			if memErr != nil {
				dbg.Printf(" => Memory err: %s", memErr)
				return nil, firstErr(err, ErrMemoryOutOfBounds)
			}
			data = sha3.Sha3(data)
			stack.PushBytes(data)
			dbg.Printf(" => (%v) %X\n", size, data)

		case ADDRESS: // 0x30
			stack.Push(callee.Address)
			dbg.Printf(" => %X\n", callee.Address)

		case BALANCE: // 0x31
			addr := stack.Pop()
			if useGasNegative(gas, GasGetAccount, &err) {
				return nil, err
			}
			acc := vm.appState.GetAccount(addr)
			if acc == nil {
				return nil, firstErr(err, ErrUnknownAddress)
			}
			balance := acc.Balance
			stack.Push64(balance)
			dbg.Printf(" => %v (%X)\n", balance, addr)

		case ORIGIN: // 0x32
			stack.Push(vm.origin)
			dbg.Printf(" => %X\n", vm.origin)

		case CALLER: // 0x33
			stack.Push(caller.Address)
			dbg.Printf(" => %X\n", caller.Address)

		case CALLVALUE: // 0x34
			stack.Push64(value)
			dbg.Printf(" => %v\n", value)

		case CALLDATALOAD: // 0x35
			offset := stack.Pop64()
			data, ok := subslice(input, offset, 32)
			if !ok {
				return nil, firstErr(err, ErrInputOutOfBounds)
			}
			res := LeftPadWord256(data)
			stack.Push(res)
			dbg.Printf(" => 0x%X\n", res)

		case CALLDATASIZE: // 0x36
			stack.Push64(int64(len(input)))
			dbg.Printf(" => %d\n", len(input))

		case CALLDATACOPY: // 0x37
			memOff := stack.Pop64()
			inputOff := stack.Pop64()
			length := stack.Pop64()
			data, ok := subslice(input, inputOff, length)
			if !ok {
				return nil, firstErr(err, ErrInputOutOfBounds)
			}
			memErr := memory.Write(memOff, data)
			if memErr != nil {
				dbg.Printf(" => Memory err: %s", memErr)
				return nil, firstErr(err, ErrMemoryOutOfBounds)
			}
			dbg.Printf(" => [%v, %v, %v] %X\n", memOff, inputOff, length, data)

		case CODESIZE: // 0x38
			l := int64(len(code))
			stack.Push64(l)
			dbg.Printf(" => %d\n", l)

		case CODECOPY: // 0x39
			memOff := stack.Pop64()
			codeOff := stack.Pop64()
			length := stack.Pop64()
			data, ok := subslice(code, codeOff, length)
			if !ok {
				return nil, firstErr(err, ErrCodeOutOfBounds)
			}
			memErr := memory.Write(memOff, data)
			if memErr != nil {
				dbg.Printf(" => Memory err: %s", memErr)
				return nil, firstErr(err, ErrMemoryOutOfBounds)
			}
			dbg.Printf(" => [%v, %v, %v] %X\n", memOff, codeOff, length, data)

		case GASPRICE_DEPRECATED: // 0x3A
			stack.Push(Zero256)
			dbg.Printf(" => %X (GASPRICE IS DEPRECATED)\n")

		case EXTCODESIZE: // 0x3B
			addr := stack.Pop()
			if useGasNegative(gas, GasGetAccount, &err) {
				return nil, err
			}
			acc := vm.appState.GetAccount(addr)
			if acc == nil {
				if _, ok := registeredNativeContracts[addr]; !ok {
					return nil, firstErr(err, ErrUnknownAddress)
				}
				dbg.Printf(" => returning code size of 1 to indicated existence of native contract at %X\n", addr)
				stack.Push(One256)
			} else {
				code := acc.Code
				l := int64(len(code))
				stack.Push64(l)
				dbg.Printf(" => %d\n", l)
			}
		case EXTCODECOPY: // 0x3C
			addr := stack.Pop()
			if useGasNegative(gas, GasGetAccount, &err) {
				return nil, err
			}
			acc := vm.appState.GetAccount(addr)
			if acc == nil {
				if _, ok := registeredNativeContracts[addr]; ok {
					dbg.Printf(" => attempted to copy native contract at %X but this is not supported\n", addr)
					return nil, firstErr(err, ErrNativeContractCodeCopy)
				}
				return nil, firstErr(err, ErrUnknownAddress)
			}
			code := acc.Code
			memOff := stack.Pop64()
			codeOff := stack.Pop64()
			length := stack.Pop64()
			data, ok := subslice(code, codeOff, length)
			if !ok {
				return nil, firstErr(err, ErrCodeOutOfBounds)
			}
			memErr := memory.Write(memOff, data)
			if memErr != nil {
				dbg.Printf(" => Memory err: %s", memErr)
				return nil, firstErr(err, ErrMemoryOutOfBounds)
			}
			dbg.Printf(" => [%v, %v, %v] %X\n", memOff, codeOff, length, data)

		case BLOCKHASH: // 0x40
			stack.Push(Zero256)
			dbg.Printf(" => 0x%X (NOT SUPPORTED)\n", stack.Peek().Bytes())

		case COINBASE: // 0x41
			stack.Push(Zero256)
			dbg.Printf(" => 0x%X (NOT SUPPORTED)\n", stack.Peek().Bytes())

		case TIMESTAMP: // 0x42
			time := vm.params.BlockTime
			stack.Push64(int64(time))
			dbg.Printf(" => 0x%X\n", time)

		case BLOCKHEIGHT: // 0x43
			number := int64(vm.params.BlockHeight)
			stack.Push64(number)
			dbg.Printf(" => 0x%X\n", number)

		case GASLIMIT: // 0x45
			stack.Push64(vm.params.GasLimit)
			dbg.Printf(" => %v\n", vm.params.GasLimit)

		case POP: // 0x50
			popped := stack.Pop()
			dbg.Printf(" => 0x%X\n", popped)

		case MLOAD: // 0x51
			offset := stack.Pop64()
			data, memErr := memory.Read(offset, 32)
			if memErr != nil {
				dbg.Printf(" => Memory err: %s", memErr)
				return nil, firstErr(err, ErrMemoryOutOfBounds)
			}
			stack.Push(LeftPadWord256(data))
			dbg.Printf(" => 0x%X @ 0x%X\n", data, offset)

		case MSTORE: // 0x52
			offset, data := stack.Pop64(), stack.Pop()
			memErr := memory.Write(offset, data.Bytes())
			if memErr != nil {
				dbg.Printf(" => Memory err: %s", memErr)
				return nil, firstErr(err, ErrMemoryOutOfBounds)
			}
			dbg.Printf(" => 0x%X @ 0x%X\n", data, offset)

		case MSTORE8: // 0x53
			offset, val := stack.Pop64(), byte(stack.Pop64()&0xFF)
			memErr := memory.Write(offset, []byte{val})
			if memErr != nil {
				dbg.Printf(" => Memory err: %s", memErr)
				return nil, firstErr(err, ErrMemoryOutOfBounds)
			}
			dbg.Printf(" => [%v] 0x%X\n", offset, val)

		case SLOAD: // 0x54
			loc := stack.Pop()
			data := vm.appState.GetStorage(callee.Address, loc)
			stack.Push(data)
			dbg.Printf(" {0x%X : 0x%X}\n", loc, data)

		case SSTORE: // 0x55
			loc, data := stack.Pop(), stack.Pop()
			if useGasNegative(gas, GasStorageUpdate, &err) {
				return nil, err
			}
			vm.appState.SetStorage(callee.Address, loc, data)
			dbg.Printf(" {0x%X : 0x%X}\n", loc, data)

		case JUMP: // 0x56
			if err = jump(code, stack.Pop64(), &pc); err != nil {
				return nil, err
			}
			continue

		case JUMPI: // 0x57
			pos, cond := stack.Pop64(), stack.Pop()
			if !cond.IsZero() {
				if err = jump(code, pos, &pc); err != nil {
					return nil, err
				}
				continue
			}
			dbg.Printf(" ~> false\n")

		case PC: // 0x58
			stack.Push64(pc)

		case MSIZE: // 0x59
			// Note: Solidity will write to this offset expecting to find guaranteed
			// free memory to be allocated for it if a subsequent MSTORE is made to
			// this offset.
			capacity := memory.Capacity()
			stack.Push64(capacity)
			dbg.Printf(" => 0x%X\n", capacity)

		case GAS: // 0x5A
			stack.Push64(*gas)
			dbg.Printf(" => %X\n", *gas)

		case JUMPDEST: // 0x5B
			dbg.Printf("\n")
			// Do nothing

		case PUSH1, PUSH2, PUSH3, PUSH4, PUSH5, PUSH6, PUSH7, PUSH8, PUSH9, PUSH10, PUSH11, PUSH12, PUSH13, PUSH14, PUSH15, PUSH16, PUSH17, PUSH18, PUSH19, PUSH20, PUSH21, PUSH22, PUSH23, PUSH24, PUSH25, PUSH26, PUSH27, PUSH28, PUSH29, PUSH30, PUSH31, PUSH32:
			a := int64(op - PUSH1 + 1)
			codeSegment, ok := subslice(code, pc+1, a)
			if !ok {
				return nil, firstErr(err, ErrCodeOutOfBounds)
			}
			res := LeftPadWord256(codeSegment)
			stack.Push(res)
			pc += a
			dbg.Printf(" => 0x%X\n", res)
			//stack.Print(10)

		case DUP1, DUP2, DUP3, DUP4, DUP5, DUP6, DUP7, DUP8, DUP9, DUP10, DUP11, DUP12, DUP13, DUP14, DUP15, DUP16:
			n := int(op - DUP1 + 1)
			stack.Dup(n)
			dbg.Printf(" => [%d] 0x%X\n", n, stack.Peek().Bytes())

		case SWAP1, SWAP2, SWAP3, SWAP4, SWAP5, SWAP6, SWAP7, SWAP8, SWAP9, SWAP10, SWAP11, SWAP12, SWAP13, SWAP14, SWAP15, SWAP16:
			n := int(op - SWAP1 + 2)
			stack.Swap(n)
			dbg.Printf(" => [%d] %X\n", n, stack.Peek())
			//stack.Print(10)

		case LOG0, LOG1, LOG2, LOG3, LOG4:
			n := int(op - LOG0)
			topics := make([]Word256, n)
			offset, size := stack.Pop64(), stack.Pop64()
			for i := 0; i < n; i++ {
				topics[i] = stack.Pop()
			}
			data, memErr := memory.Read(offset, size)
			if memErr != nil {
				dbg.Printf(" => Memory err: %s", memErr)
				return nil, firstErr(err, ErrMemoryOutOfBounds)
			}
			if vm.evc != nil {
				eventID := txs.EventStringLogEvent(callee.Address.Postfix(20))
				fmt.Printf("eventID: %s\n", eventID)
				log := txs.EventDataLog{
					callee.Address,
					topics,
					data,
					vm.params.BlockHeight,
				}
				vm.evc.FireEvent(eventID, log)
			}
			dbg.Printf(" => T:%X D:%X\n", topics, data)

		case CREATE: // 0xF0
			if !HasPermission(vm.appState, callee, ptypes.CreateContract) {
				return nil, ErrPermission{"create_contract"}
			}
			contractValue := stack.Pop64()
			offset, size := stack.Pop64(), stack.Pop64()
			input, memErr := memory.Read(offset, size)
			if memErr != nil {
				dbg.Printf(" => Memory err: %s", memErr)
				return nil, firstErr(err, ErrMemoryOutOfBounds)
			}

			// Check balance
			if callee.Balance < contractValue {
				return nil, firstErr(err, ErrInsufficientBalance)
			}

			// TODO charge for gas to create account _ the code length * GasCreateByte
			newAccount := vm.appState.CreateAccount(callee)

			// Run the input to get the contract code.
			// NOTE: no need to copy 'input' as per Call contract.
			ret, err_ := vm.Call(callee, newAccount, input, input, contractValue, gas)
			if err_ != nil {
				stack.Push(Zero256)
			} else {
				newAccount.Code = ret // Set the code (ret need not be copied as per Call contract)
				stack.Push(newAccount.Address)
			}

		case CALL, CALLCODE, DELEGATECALL: // 0xF1, 0xF2, 0xF4
			if !HasPermission(vm.appState, callee, ptypes.Call) {
				return nil, ErrPermission{"call"}
			}
			gasLimit := stack.Pop64()
			addr := stack.Pop()
			// NOTE: for DELEGATECALL value is preserved from the original
			// caller, as such it is not stored on stack as an argument
			// for DELEGATECALL and should not be popped.  Instead previous
			// caller value is used.  for CALL and CALLCODE value is stored
			// on stack and needs to be overwritten from the given value.
			if op != DELEGATECALL {
				value = stack.Pop64()
			}
			inOffset, inSize := stack.Pop64(), stack.Pop64()   // inputs
			retOffset, retSize := stack.Pop64(), stack.Pop64() // outputs
			dbg.Printf(" => %X\n", addr)

			// Get the arguments from the memory
			args, memErr := memory.Read(inOffset, inSize)
			if memErr != nil {
				dbg.Printf(" => Memory err: %s", memErr)
				return nil, firstErr(err, ErrMemoryOutOfBounds)
			}

			// Ensure that gasLimit is reasonable
			if *gas < gasLimit {
				return nil, firstErr(err, ErrInsufficientGas)
			} else {
				*gas -= gasLimit
				// NOTE: we will return any used gas later.
			}

			// Begin execution
			var ret []byte
			var err error
			if nativeContract := registeredNativeContracts[addr]; nativeContract != nil {
				// Native contract
				ret, err = nativeContract(vm.appState, callee, args, &gasLimit)

				// for now we fire the Call event. maybe later we'll fire more particulars
				var exception string
				if err != nil {
					exception = err.Error()
				}
				// NOTE: these fire call events and not particular events for eg name reg or permissions
				vm.fireCallEvent(&exception, &ret, callee, &Account{Address: addr}, args, value, &gasLimit)
			} else {
				// EVM contract
				if useGasNegative(gas, GasGetAccount, &err) {
					return nil, err
				}
				acc := vm.appState.GetAccount(addr)
				// since CALL is used also for sending funds,
				// acc may not exist yet. This is an error for
				// CALLCODE, but not for CALL, though I don't think
				// ethereum actually cares
				if op == CALLCODE {
					if acc == nil {
						return nil, firstErr(err, ErrUnknownAddress)
					}
					ret, err = vm.Call(callee, callee, acc.Code, args, value, &gasLimit)
				} else if op == DELEGATECALL {
					if acc == nil {
						return nil, firstErr(err, ErrUnknownAddress)
					}
					ret, err = vm.DelegateCall(caller, callee, acc.Code, args, value, &gasLimit)
				} else {
					// nil account means we're sending funds to a new account
					if acc == nil {
						if !HasPermission(vm.appState, caller, ptypes.CreateAccount) {
							return nil, ErrPermission{"create_account"}
						}
						acc = &Account{Address: addr}
					}
					// add account to the tx cache
					vm.appState.UpdateAccount(acc)
					ret, err = vm.Call(callee, acc, acc.Code, args, value, &gasLimit)
				}
			}

			// Push result
			if err != nil {
				dbg.Printf("error on call: %s\n", err.Error())
				stack.Push(Zero256)
			} else {
				stack.Push(One256)

				// Should probably only be necessary when there is no return value and
				// ret is empty, but since EVM expects retSize to be respected this will
				// defensively pad or truncate the portion of ret to be returned.
				memErr := memory.Write(retOffset, RightPadBytes(ret, int(retSize)))
				if memErr != nil {
					dbg.Printf(" => Memory err: %s", memErr)
					return nil, firstErr(err, ErrMemoryOutOfBounds)
				}
			}

			// Handle remaining gas.
			*gas += gasLimit

			dbg.Printf("resume %X (%v)\n", callee.Address, gas)

		case RETURN: // 0xF3
			offset, size := stack.Pop64(), stack.Pop64()
			output, memErr := memory.Read(offset, size)
			if memErr != nil {
				dbg.Printf(" => Memory err: %s", memErr)
				return nil, firstErr(err, ErrMemoryOutOfBounds)
			}
			dbg.Printf(" => [%v, %v] (%d) 0x%X\n", offset, size, len(output), output)
			return output, nil

		case SELFDESTRUCT: // 0xFF
			addr := stack.Pop()
			if useGasNegative(gas, GasGetAccount, &err) {
				return nil, err
			}
			// TODO if the receiver is , then make it the fee. (?)
			// TODO: create account if doesn't exist (no reason not to)
			receiver := vm.appState.GetAccount(addr)
			if receiver == nil {
				return nil, firstErr(err, ErrUnknownAddress)
			}
			balance := callee.Balance
			receiver.Balance += balance
			vm.appState.UpdateAccount(receiver)
			vm.appState.RemoveAccount(callee)
			dbg.Printf(" => (%X) %v\n", addr[:4], balance)
			fallthrough

		case STOP: // 0x00
			return nil, nil

		default:
			dbg.Printf("(pc) %-3v Invalid opcode %X\n", pc, op)
			return nil, fmt.Errorf("Invalid opcode %X", op)
		}

		pc++
	}
}

// TODO: [Silas] this function seems extremely dubious to me. It was being used
// in circumstances where its behaviour did not match the intention. It's bounds
// check is strange (treats a read at data length as a zero read of arbitrary length)
// I have left it in for now to be conservative about where its behaviour is being used
//
// Returns a subslice from offset of length length and a bool
// (true iff slice was possible). If the subslice
// extends past the end of data it returns A COPY of the segment at the end of
// data padded with zeroes on the right. If offset == len(data) it returns all
// zeroes. if offset > len(data) it returns a false
func subslice(data []byte, offset, length int64) (ret []byte, ok bool) {
	size := int64(len(data))
	if size < offset {
		return nil, false
	} else if size < offset+length {
		ret, ok = data[offset:], true
		ret = RightPadBytes(ret, 32)
	} else {
		ret, ok = data[offset:offset+length], true
	}
	return
}

func codeGetOp(code []byte, n int64) OpCode {
	if int64(len(code)) <= n {
		return OpCode(0) // stop
	} else {
		return OpCode(code[n])
	}
}

func jump(code []byte, to int64, pc *int64) (err error) {
	dest := codeGetOp(code, to)
	if dest != JUMPDEST {
		dbg.Printf(" ~> %v invalid jump dest %v\n", to, dest)
		return ErrInvalidJumpDest
	}
	dbg.Printf(" ~> %v\n", to)
	*pc = to
	return nil
}

func firstErr(errA, errB error) error {
	if errA != nil {
		return errA
	} else {
		return errB
	}
}

func transfer(from, to *Account, amount int64) error {
	if from.Balance < amount {
		return ErrInsufficientBalance
	} else {
		from.Balance -= amount
		to.Balance += amount
		return nil
	}
}
