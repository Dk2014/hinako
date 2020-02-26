package hinako

import (
	"syscall"

	"fmt"
	"reflect"
	"strings"
	"unsafe"

	"golang.org/x/arch/x86/x86asm"
)

var (
	kernel32              = syscall.NewLazyDLL("kernel32.dll")
	virtualAlloc          = kernel32.NewProc("VirtualAlloc")
	virtualFree           = kernel32.NewProc("VirtualFree")
	virtualProtect        = kernel32.NewProc("VirtualProtect")
	flushInstructionCache = kernel32.NewProc("FlushInstructionCache")
)

const (
	_MEM_COMMIT  = 0x00001000
	_MEM_RELEASE = 0x8000
)

func unlockMemoryProtect(addr uintptr, size int, fun func() error) error {
	oldProtect, err := changeMemoryProtectLevel(addr, size, syscall.PAGE_EXECUTE_READWRITE)
	if err != nil {
		return err
	}
	defer func() {
		if _, err := changeMemoryProtectLevel(addr, size, oldProtect); err != nil {
			panic(err)
		}
	}()
	return fun()
}

func changeMemoryProtectLevel(ptr uintptr, size, protectLevel int) (int, error) {
	oldProtectLevel := 0
	if r, _, err := virtualProtect.Call(ptr, uintptr(size), uintptr(protectLevel), uintptr(unsafe.Pointer(&oldProtectLevel))); r == 0 {
		return -1, err
	}
	return oldProtectLevel, nil
}

func unsafeReadMemory(ptr uintptr, out []byte) error {
	for i := range out {
		out[i] = *(*byte)(unsafe.Pointer(ptr + uintptr(i)))
	}
	// todo: error handling
	return nil
}

func unsafeWriteMemory(ptr uintptr, in []byte) error {
	for i, b := range in {
		*(*byte)(unsafe.Pointer(ptr + uintptr(i))) = b
	}
	// todo: error handling
	return nil
}

type Hook struct {
	Arch         Arch
	OriginalProc *syscall.Proc
	HookFunc     interface{}

	targetProc *syscall.Proc
	trampoline *virtualAllocatedMemory
	patchSize  int
}

func (h *Hook) Close() {
	if h.trampoline == nil {
		return
	}
	defer h.trampoline.Close()

	// revert jump patch
	patch := make([]byte, h.patchSize)
	err := unsafeReadMemory(h.trampoline.Addr, patch)
	if err != nil {
		panic(err)
	}

	if err := unlockMemoryProtect(h.targetProc.Addr(), len(patch), func() error {
		return unsafeWriteMemory(h.targetProc.Addr(), patch)
	}); err != nil {
		panic(err)
	}
}

func NewHookByName(arch Arch, dllName, funcName string, hookFunc interface{}) (*Hook, error) {
	dll, err := syscall.LoadDLL(dllName)
	if err != nil {
		return nil, err
	}
	targetProc, err := dll.FindProc(funcName)
	if err != nil {
		return nil, err
	}

	hook, err := NewHook(arch, targetProc, hookFunc)
	if err != nil {
		return nil, err
	}
	return hook, nil
}

func NewHook(arch Arch, targetProc *syscall.Proc, hookFunc interface{}) (*Hook, error) {
	// todo: already hooked?
	targetFuncAddr := targetProc.Addr()
	hookFuncCallbackAddr := syscall.NewCallback(hookFunc)

	originalFuncHead := make([]byte, 20)
	if err := unsafeReadMemory(targetFuncAddr, originalFuncHead); err != nil {
		return nil, err
	}

	insts, err := disassemble(originalFuncHead, arch.DisassembleMode())
	if err != nil {
		return nil, err
	}

	jumpSize := jumpSize(arch, targetFuncAddr, hookFuncCallbackAddr)
	patchSize, err := getAsmPatchSize(insts, jumpSize)
	if err != nil {
		return nil, err
	}
	// printDisas(arch, targetFuncAddr, 20, "original func head")

	currentProcessHandle, err := syscall.GetCurrentProcess()
	if err != nil {
		return nil, err
	}

	// allocate trampoline buffer
	tramp, err := newVirtualAllocatedMemory(maxTrampolineSize(arch), syscall.PAGE_EXECUTE_READWRITE)
	if err != nil {
		return nil, err
	}

	// copy head of original function to trampoline
	if _, err := tramp.Write(originalFuncHead[:patchSize]); err != nil {
		return nil, err
	}
	// printDisas(arch, tramp.Addr, int(tramp.Size), "tramp func")

	// add jump to original function tail opcode to trampoline
	jmp := newJumpAsm(arch, tramp.Addr+uintptr(patchSize), targetFuncAddr+uintptr(patchSize))
	if _, err = tramp.WriteAt(jmp, int64(patchSize)); err != nil {
		return nil, err
	}
	if r, _, err := flushInstructionCache.Call(uintptr(currentProcessHandle), tramp.Addr, uintptr(tramp.Size)); r == 0 {
		return nil, err
	}
	// printDisas(arch, tramp.Addr, int(tramp.Size), "tramp func")

	if err := unlockMemoryProtect(targetFuncAddr, patchSize, func() error {
		// overwrite head of target func with jumping for hook func
		hookJmp := newJumpAsm(arch, targetFuncAddr, hookFuncCallbackAddr)
		if err := unsafeWriteMemory(targetFuncAddr, hookJmp); err != nil {
			return err
		}
		if r, _, err := flushInstructionCache.Call(uintptr(currentProcessHandle), targetFuncAddr, uintptr(patchSize)); r == 0 {
			return err
		}
		// printDisas(arch, targetFuncAddr, 20, "original func head (after patched)")
		return nil
	}); err != nil {
		return nil, err
	}

	originalProc := &syscall.Proc{Dll: targetProc.Dll, Name: targetProc.Name}
	// HACK: overwrite Proc.addr with trampoline address
	*(*uintptr)(unsafe.Pointer(reflect.Indirect(reflect.ValueOf(originalProc)).FieldByName("addr").UnsafeAddr())) = tramp.Addr

	return &Hook{
		Arch:         arch,
		OriginalProc: originalProc,
		HookFunc:     hookFunc,
		targetProc:   targetProc,
		trampoline:   tramp,
		patchSize:    patchSize,
	}, nil
}

func getAsmPatchSize(insts []*x86asm.Inst, jumpSize uint) (int, error) {
	res := 0
	for i := 0; res < int(jumpSize) && i < len(insts); i++ {
		if isBranchInst(insts[i]) {
			return -1, fmt.Errorf("Branch opcode found before jump patch area")
		}
		res += insts[i].Len
	}
	if res < int(jumpSize) {
		return -1, fmt.Errorf("Unable to insert jmp within patch size")
	}
	return res, nil
}

func isBranchInst(inst *x86asm.Inst) bool {
	instr := inst.String()
	return strings.HasPrefix(instr, "J") || strings.HasPrefix(instr, "CALL") || strings.HasPrefix(instr, "RET")
}

func disassemble(src []byte, mode int) ([]*x86asm.Inst, error) {
	var r []*x86asm.Inst
	for len(src) > 0 {
		inst, err := x86asm.Decode(src, mode)
		if err != nil {
			return nil, err
		}
		r = append(r, &inst)
		src = src[inst.Len:]
	}
	return r, nil
}
