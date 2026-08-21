//go:build windows

package run

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"
)

const (
	createSuspended           = 0x00000004
	jobObjectBasicLimitInfo   = 2
	jobObjectLimitKillOnClose = 0x00002000
	processSetQuota           = 0x00000100
	threadSuspendResume       = 0x00000002
	invalidResumeCount        = 0xffffffff
)

var (
	processKernel32          = syscall.NewLazyDLL("kernel32.dll")
	createJobObjectW         = processKernel32.NewProc("CreateJobObjectW")
	setInformationJobObject  = processKernel32.NewProc("SetInformationJobObject")
	assignProcessToJobObject = processKernel32.NewProc("AssignProcessToJobObject")
	terminateJobObject       = processKernel32.NewProc("TerminateJobObject")
	thread32First            = processKernel32.NewProc("Thread32First")
	thread32Next             = processKernel32.NewProc("Thread32Next")
	openThread               = processKernel32.NewProc("OpenThread")
	resumeThread             = processKernel32.NewProc("ResumeThread")
)

type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type threadEntry32 struct {
	Size           uint32
	Usage          uint32
	ThreadID       uint32
	OwnerProcessID uint32
	BasePri        int32
	DeltaPri       int32
	Flags          uint32
}

type processTree struct {
	mu         sync.Mutex
	job        syscall.Handle
	assigned   bool
	terminated bool
}

func prepareProcessTree(cmd *exec.Cmd) (*processTree, error) {
	job, _, callErr := createJobObjectW.Call(0, 0)
	if job == 0 {
		return nil, windowsCallError("create process job", callErr)
	}
	tree := &processTree{job: syscall.Handle(job)}
	limits := jobObjectBasicLimitInformation{LimitFlags: jobObjectLimitKillOnClose}
	result, _, callErr := setInformationJobObject.Call(
		job,
		jobObjectBasicLimitInfo,
		uintptr(unsafe.Pointer(&limits)),
		unsafe.Sizeof(limits),
	)
	if result == 0 {
		_ = syscall.CloseHandle(tree.job)
		return nil, windowsCallError("configure process job", callErr)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createSuspended}
	return tree, nil
}

func (tree *processTree) afterStart(process *os.Process) error {
	tree.mu.Lock()
	defer tree.mu.Unlock()
	if process == nil || process.Pid <= 0 {
		return errors.New("started process is unavailable")
	}
	if tree.terminated {
		return os.ErrProcessDone
	}
	processHandle, err := syscall.OpenProcess(processSetQuota|syscall.PROCESS_TERMINATE, false, uint32(process.Pid))
	if err != nil {
		return fmt.Errorf("open tool process: %w", err)
	}
	defer syscall.CloseHandle(processHandle)
	result, _, callErr := assignProcessToJobObject.Call(uintptr(tree.job), uintptr(processHandle))
	if result == 0 {
		return windowsCallError("assign tool process to job", callErr)
	}
	tree.assigned = true
	if err := resumeProcessThread(uint32(process.Pid)); err != nil {
		_, _, _ = terminateJobObject.Call(uintptr(tree.job), 1)
		tree.terminated = true
		return err
	}
	return nil
}

func (tree *processTree) terminate(process *os.Process) error {
	tree.mu.Lock()
	defer tree.mu.Unlock()
	tree.terminated = true
	if tree.job != 0 && tree.assigned {
		result, _, callErr := terminateJobObject.Call(uintptr(tree.job), 1)
		if result != 0 {
			return nil
		}
		if process != nil {
			_ = process.Kill()
		}
		return windowsCallError("terminate tool process job", callErr)
	}
	if process == nil {
		return os.ErrProcessDone
	}
	return process.Kill()
}

func (tree *processTree) close(process *os.Process) error {
	tree.mu.Lock()
	defer tree.mu.Unlock()
	if tree.job == 0 {
		return nil
	}
	var terminateErr error
	if tree.assigned {
		result, _, callErr := terminateJobObject.Call(uintptr(tree.job), 1)
		if result == 0 {
			terminateErr = windowsCallError("terminate tool process job", callErr)
			if process != nil {
				_ = process.Kill()
			}
		}
	}
	closeErr := syscall.CloseHandle(tree.job)
	tree.job = 0
	return errors.Join(terminateErr, closeErr)
}

func resumeProcessThread(processID uint32) error {
	snapshot, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("snapshot tool threads: %w", err)
	}
	defer syscall.CloseHandle(snapshot)
	entry := threadEntry32{Size: uint32(unsafe.Sizeof(threadEntry32{}))}
	result, _, callErr := thread32First.Call(uintptr(snapshot), uintptr(unsafe.Pointer(&entry)))
	for result != 0 {
		if entry.OwnerProcessID == processID {
			thread, _, openErr := openThread.Call(threadSuspendResume, 0, uintptr(entry.ThreadID))
			if thread == 0 {
				return windowsCallError("open tool thread", openErr)
			}
			resumed, _, resumeErr := resumeThread.Call(thread)
			closeErr := syscall.CloseHandle(syscall.Handle(thread))
			if resumed == invalidResumeCount {
				return errors.Join(windowsCallError("resume tool thread", resumeErr), closeErr)
			}
			return closeErr
		}
		entry.Size = uint32(unsafe.Sizeof(threadEntry32{}))
		result, _, callErr = thread32Next.Call(uintptr(snapshot), uintptr(unsafe.Pointer(&entry)))
	}
	if !errors.Is(callErr, syscall.ERROR_NO_MORE_FILES) {
		return windowsCallError("enumerate tool threads", callErr)
	}
	return errors.New("tool process thread was not found")
}

func windowsCallError(operation string, err error) error {
	if err == nil || errors.Is(err, syscall.Errno(0)) {
		err = syscall.EINVAL
	}
	return fmt.Errorf("%s: %w", operation, err)
}
