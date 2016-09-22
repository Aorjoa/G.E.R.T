package main

import "fmt"
import "math/rand"
import "runtime"
import "runtime/debug"
import "sync/atomic"
import "sync"
import "time"
import "unsafe"

type trapstore_t struct {
	trapno    uintptr
	faultaddr uintptr
	tf        [TFSIZE]uintptr
	inttime   int
}
const maxtstore int = 64
const maxcpus   int = 32

//go:nosplit
func tsnext(c int) int {
	return (c + 1) % maxtstore
}

var	numcpus	int = 1

type cpu_t struct {
	// logical number, not lapic id
	num		int
	// per-cpus interrupt queues. the cpu interrupt handler is the
	// producer, the go routine running trap() below is the consumer. each
	// cpus interrupt handler increments head while the go routine consumer
	// increments tail
	trapstore	[maxtstore]trapstore_t
	tshead		int
	tstail		int
}

var cpus	[maxcpus]cpu_t

// these functions can only be used when interrupts are cleared
//go:nosplit
func lap_id() int {
	lapaddr := (*[1024]uint32)(unsafe.Pointer(uintptr(0xfee00000)))
	return int(lapaddr[0x20/4] >> 24)
}

const(
	DIVZERO		= 0
	UD		= 6
	GPFAULT		= 13
	PGFAULT		= 14
	TIMER		= 32
	SYSCALL		= 64
	TLBSHOOT	= 70

	// low 3 bits must be zero
	IRQ_BASE	= 32
	IRQ_KBD		= 1
	IRQ_COM1	= 4
	IRQ_LAST	= IRQ_BASE + 16

	INT_KBD		= IRQ_BASE + IRQ_KBD
	INT_COM1	= IRQ_BASE + IRQ_COM1
)

// initialized by disk attach functions
var IRQ_DISK	int = -1
var INT_DISK	int = -1

// trap cannot do anything that may have side-effects on the runtime (like
// fmt.Print, or use panic!). the reason is that goroutines are scheduled
// cooperatively in the runtime. trap interrupts the runtime though, and then
// tries to execute more gocode on the same M, thus doing things the runtime
// did not expect.
//go:nosplit
func trapstub(tf *[TFSIZE]uintptr) {

	lid := cpus[lap_id()].num
	head := cpus[lid].tshead
	tail := cpus[lid].tstail
	ts := &cpus[lid].trapstore[head]

	// make sure circular buffer has room
	if tsnext(head) == tail {
		for i := tail; i != head; i = tsnext(i) {
			runtime.Pnum(int(cpus[lid].trapstore[i].trapno))
		}
		runtime.Pnum(0xbad)
		for {}
	}

	// extract process and thread id
	ts.inttime = runtime.Nanotime()

	trapno := tf[TF_TRAP]

	// only IRQs come through here now
	if trapno <= TIMER {
		runtime.Pnum(0x1001)
		for {}
	}

	// add to trap circular buffer for actual trap handler
	ts.trapno = trapno
	ts.tf = *tf
	if trapno == PGFAULT {
		ts.faultaddr = runtime.Rcr2()
	}

	// commit interrupt
	head = tsnext(head)
	cpus[lid].tshead = head

	runtime.Trapwake()

	switch trapno {
	case uintptr(INT_DISK), INT_KBD, INT_COM1:
		// we need to mask the interrupt on the IOAPIC since my
		// hardware's LAPIC automatically send EOIs to IOAPICS when the
		// LAPIC receives an EOI and does not support disabling these
		// automatic EOI broadcasts (newer LAPICs do). its probably
		// better to disable automatic EOI broadcast and send EOIs to
		// the IOAPICs in the driver (as the code used to when using
		// 8259s).
		// masking the IRQ on the IO APIC must happen before writing
		// EOI to the LAPIC (otherwise the CPU will probably receive
		// another interrupt from the same IRQ). the LAPIC EOI happens
		// in the runtime...
		irqno := int(trapno - IRQ_BASE)
		apic.irq_mask(irqno)
	default:
		// unexpected IRQ
		runtime.Pnum(int(trapno))
		runtime.Pnum(int(tf[TF_RIP]))
		runtime.Pnum(0xbadbabe)
		for {}
	}
}

func trap(handlers map[int]func(*trapstore_t)) {
	runtime.Trapinit()
	for {
		for cpu := 0; cpu < numcpus; {
			head := cpus[cpu].tshead
			tail := cpus[cpu].tstail

			if tail == head {
				// no work for this cpu
				cpu++
				continue
			}

			tcur := trapstore_t{}
			tcur = cpus[cpu].trapstore[tail]

			trapno := tcur.trapno

			tail = tsnext(tail)
			cpus[cpu].tstail = tail

			if h, ok := handlers[int(trapno)]; ok {
				go h(&tcur)
				continue
			}
			panic(fmt.Sprintf("no handler for trap %v\n", trapno))
		}
		runtime.Trapsched()
	}
}

func trap_disk(ts *trapstore_t) {
	// is this a disk int?
	if !disk.intr() {
		fmt.Printf("spurious disk int\n")
		return
	}
	ide_int_done <- true
}

func trap_cons(ts *trapstore_t) {
	var ch chan bool
	if ts.trapno == INT_KBD {
		ch = cons.kbd_int
	} else if ts.trapno == INT_COM1 {
		ch = cons.com_int
	} else {
		panic("bad int")
	}
	ch <- true
}

func tfdump(tf *[TFSIZE]int) {
	fmt.Printf("RIP: %#x\n", tf[TF_RIP])
	fmt.Printf("RAX: %#x\n", tf[TF_RAX])
	fmt.Printf("RDI: %#x\n", tf[TF_RDI])
	fmt.Printf("RSI: %#x\n", tf[TF_RSI])
	fmt.Printf("RBX: %#x\n", tf[TF_RBX])
	fmt.Printf("RCX: %#x\n", tf[TF_RCX])
	fmt.Printf("RDX: %#x\n", tf[TF_RDX])
	fmt.Printf("RSP: %#x\n", tf[TF_RSP])
}

// XXX
func cdelay(n int) {
	for i := 0; i < n*1000000; i++ {
	}
}

type dev_t struct {
	major	int
	minor	int
}

// allocated device major numbers
// internally, biscuit uses device numbers for all special, on-disk files.
const(
	D_CONSOLE int	= 1
	// UNIX domain sockets
	D_SUD 		= 2
	D_SUS 		= 3
	D_DEVNULL	= 4
	D_FIRST		= D_CONSOLE
	D_LAST		= D_SUS
)

// threads/processes can concurrently call a single fd's methods
type fdops_i interface {
	// fd ops
	close() int
	fstat(*stat_t) int
	lseek(int, int) int
	mmapi(int, int) ([]mmapinfo_t, int)
	pathi() *imemnode_t
	read(*userbuf_t) (int, int)
	// reopen() is called with proc_t.fdl is held
	reopen() int
	write(*userbuf_t) (int, int)
	fullpath() (string, int)
	truncate(uint) int

	pread(*userbuf_t, int) (int, int)
	pwrite(*userbuf_t, int) (int, int)

	// socket ops
	// returns fops of new fd, size of connector's address written to user
	// space, and error
	accept(*proc_t, *userbuf_t) (fdops_i, int, int)
	bind(*proc_t, []uint8) int
	connect(*proc_t, []uint8) int
	// listen changes the underlying socket type; thus is returns the new
	// fops.
	listen(*proc_t, int) (fdops_i, int)
	sendto(*proc_t, *userbuf_t, []uint8, int) (int, int)
	// returns number of bytes read, size of from sock address written, and
	// error
	recvfrom(*proc_t, *userbuf_t, *userbuf_t) (int, int, int)

	// for poll/select
	// returns the current ready flags. pollone() will only cause the
	// device to send a notification if none of the states being polled are
	// currently true.
	pollone(pollmsg_t) ready_t

	fcntl(*proc_t, int, int) int
	getsockopt(*proc_t, int, *userbuf_t, int) (int, int)
}

// this is the new fd_t
type fd_t struct {
	// fops is an interface implemented via a "pointer receiver", thus fops
	// is a reference, not a value
	fops	fdops_i
	perms	int
}

const(
	FD_READ		= 0x1
	FD_WRITE	= 0x2
	FD_CLOEXEC	= 0x4
)

var dummyfops	= &devfops_t{priv: nil, maj: D_CONSOLE, min: 0}

// special fds
var fd_stdin 	= fd_t{fops: dummyfops, perms: FD_READ}
var fd_stdout 	= fd_t{fops: dummyfops, perms: FD_WRITE}
var fd_stderr 	= fd_t{fops: dummyfops, perms: FD_WRITE}

type ulimit_t struct {
	pages	int
	nofile	uint
}

// accnt_t is thread-safe
type accnt_t struct {
	// nanoseconds
	userns		int64
	sysns		int64
	// for getting consistent snapshot of both times; not always needed
	sync.Mutex
}

func (a *accnt_t) utadd(delta int) {
	atomic.AddInt64(&a.userns, int64(delta))
}

func (a *accnt_t) systadd(delta int) {
	atomic.AddInt64(&a.sysns, int64(delta))
}

func (a *accnt_t) now() int {
	return int(time.Now().UnixNano())
}

func (a *accnt_t) io_time(since int) {
	d := a.now() - since
	a.systadd(-d)
}

func (a *accnt_t) sleep_time(since int) {
	d := a.now() - since
	a.systadd(-d)
}

func (a *accnt_t) finish(inttime int) {
	a.systadd(a.now() - inttime)
}

func (a *accnt_t) add(n *accnt_t) {
	a.Lock()
	a.userns += n.userns
	a.sysns += n.sysns
	a.Unlock()
}

func (a *accnt_t) fetch() []uint8 {
	a.Lock()
	ru := a.to_rusage()
	a.Unlock()
	return ru
}

func (a *accnt_t) to_rusage() []uint8 {
	words := 4
	ret := make([]uint8, words*8)
	totv := func(nano int64) (int, int) {
		secs := int(nano/1e9)
		usecs := int((nano%1e9)/1000)
		return secs, usecs
	}
	off := 0
	// user timeval
	s, us := totv(a.userns)
	writen(ret, 8, off, s)
	off += 8
	writen(ret, 8, off, us)
	off += 8
	// sys timeval
	s, us = totv(a.sysns)
	writen(ret, 8, off, s)
	off += 8
	writen(ret, 8, off, us)
	off += 8
	return ret
}

// requirements for wait* syscalls (used for processes and threads):
// - wait for a pid that is not my child must fail
// - only one wait for a specific pid may succeed; others must fail
// - wait when there are no children must fail
// - wait for a process should not return thread info and vice versa
type waitst_t struct {
	pid		int
	err		int
	status		int
	atime		accnt_t
}

type waitent_t struct {
	waiter		chan waitst_t
	wstatus		waitst_t
	// we need this flag so that WAIT_ANY can differentiate threads from
	// procs when looking for a proc that has already terminated
	isproc		bool
	dead		bool
}

type wait_t struct {
	sync.Mutex
	ids		map[int]waitent_t
	// number of child processes (not threads)
	childs		int
	_anyhints	[]int
	anyhints	[]int
	anys		chan waitst_t
	wakeany		int
}

func (w *wait_t) wait_init() {
	w.ids = make(map[int]waitent_t, 10)
	w.anys = make(chan waitst_t)
	w._anyhints = make([]int, 0, 10)
	w.anyhints = w._anyhints
	w.wakeany = 0
	w.childs = 0
}

func (w *wait_t) _pop_hint() (int, bool) {
	if len(w.anyhints) == 0 {
		return 0, false
	}
	ret := w.anyhints[0]
	w.anyhints = w.anyhints[1:]
	if len(w.anyhints) == 0 {
		w.anyhints = w._anyhints
	}
	return ret, true
}

func (w *wait_t) _push_hint(id int) {
	w.anyhints = append(w.anyhints, id)
}

func (w *wait_t) _start(id int, isproc bool) {
	w.Lock()

	ent, ok := w.ids[id]
	if ok {
		panic("two start for same id")
	}
	// put zero value
	if isproc {
		ent.isproc = true
		w.childs++
	}
	w.ids[id] = ent
	w.Unlock()
}

// caller must have the wait_t locked. returns the number of WAIT_ANYs that
// need to be woken up.
func (w *wait_t) _orphancount() int {
	ret := 0
	// wakeup WAIT_ANYs with error if there are no procs
	if w.childs == 0 && w.wakeany != 0 {
		ret = w.wakeany
		w.wakeany = 0
	}
	return ret
}

func (w *wait_t) _orphanwake(times int) {
	if times > 0 {
		fail := waitst_t{err: -ECHILD}
		for ; times > 0; times-- {
			w.anys <- fail
		}
	}
}

// id can be a pid or a tid
func (w *wait_t) put(id, status int, atime *accnt_t) {
	w.Lock()

	ent, ok := w.ids[id]
	if !ok {
		panic("put without start")
	}

	ent.wstatus.pid = id
	ent.wstatus.err = 0
	ent.wstatus.status = status
	if atime != nil {
		ent.wstatus.atime.userns = atime.userns
		ent.wstatus.atime.sysns = atime.sysns
	}
	ent.dead = true

	// wakeup someone waiting for this pid
	var wakechan chan waitst_t
	if ent.waiter != nil {
		wakechan = ent.waiter
	// see if there are WAIT_ANYs
	} else if ent.isproc && w.wakeany != 0 {
		wakechan = w.anys
		w.wakeany--
		if w.wakeany < 0 {
			panic("nyet!")
		}
	} else {
		// no waiters, add to map so someone can later reap
		w.ids[id] = ent
		if ent.isproc {
			w._push_hint(id)
		}
	}

	if wakechan != nil {
		delete(w.ids, id)
		if ent.isproc {
			w.childs--
		}
	}
	owake := w._orphancount()

	w.Unlock()

	if wakechan != nil {
		wakechan <- ent.wstatus
	}

	w._orphanwake(owake)
}

func (w *wait_t) reap(id int, noblk bool) waitst_t {
	if id == WAIT_MYPGRP {
		panic("no imp")
	}
	block := !noblk

	w.Lock()

	var ret waitst_t
	var waitchan chan waitst_t
	var owake int

	if id == WAIT_ANY {
		if w.childs < 0 {
			panic("neg childs")
		}
		if w.childs == 0 {
			ret.err = -ECHILD
			goto out
		}
		found := false
		for hint, ok := w._pop_hint(); ok; hint, ok = w._pop_hint() {
			ent := w.ids[hint]
			if ent.dead && ent.isproc {
				ret = ent.wstatus
				delete(w.ids, hint)
				found = true
				w.childs--
				break
			}
		}
		// otherwise, wait
		if !found && block {
			w.wakeany++
			waitchan = w.anys
		}
	} else {
		ent, ok := w.ids[id]
		if !ok || ent.waiter != nil {
			ret.err = -ECHILD
			goto out
		}
		if ent.dead {
			ret = ent.wstatus
			delete(w.ids, id)
			if ent.isproc {
				w.childs--
			}
		} else if block {
			// need to wait
			waitchan = make(chan waitst_t)
			ent.waiter = waitchan
			w.ids[id] = ent
		}
	}
	owake = w._orphancount()

	w.Unlock()

	if waitchan != nil {
		ret = <- waitchan
	}

	w._orphanwake(owake)

	return ret
out:
	w.Unlock()
	return ret
}

func (w *wait_t) start_proc(id int) {
	w._start(id, true)
}

func (w *wait_t) start_thread(id tid_t) {
	w._start(int(id), false)
}

type tid_t int

type threadinfo_t struct {
	alive	map[tid_t]bool
	sync.Mutex
}

func (t *threadinfo_t) init() {
	t.alive = make(map[tid_t]bool)
}

type proc_t struct {
	pid		int
	// first thread id
	tid0		tid_t
	name		string

	// waitinfo for my child processes
	mywait		wait_t
	// waitinfo of my parent
	pwait		*wait_t

	// thread tids of this process
	threadi		threadinfo_t

	// lock for vmregion, pmpages, pmap, and p_pmap
	pgfl		sync.Mutex
	pgfltaken	bool

	vmregion	vmregion_t

	// pmap pages
	pmap		*[512]int
	p_pmap		int

	// mmap next virtual address hint
	mmapi		int

	// a process is marked doomed when it has been killed but may have
	// threads currently running on another processor
	doomed		bool
	exitstatus	int

	fds		[]*fd_t
	// where to start scanning for free fds
	fdstart		int
	// fds, fdstart protected by fdl
	fdl		sync.Mutex

	cwd		*fd_t
	// to serialize chdirs
	cwdl		sync.Mutex
	ulim		ulimit_t

	// this proc's rusage
	atime		accnt_t
	// total child rusage
	catime		accnt_t
}

var proclock = sync.Mutex{}
var allprocs = map[int]*proc_t{}

var pid_cur  int

func newpid() int {
	proclock.Lock()
	pid_cur++
	ret := pid_cur
	proclock.Unlock()

	return ret
}

var _deflimits = ulimit_t {
	// mem limit = 128 MB
	pages: (1 << 27) / (1 << 12),
	nofile: RLIM_INFINITY,
}

func proc_new(name string, cwd *fd_t, fds []*fd_t) *proc_t {
	ret := &proc_t{}

	proclock.Lock()
	pid_cur++
	np := pid_cur
	if _, ok := allprocs[np]; ok {
		panic("pid exists")
	}
	allprocs[np] = ret
	proclock.Unlock()

	ret.name = name
	ret.pid = np
	ret.fds = fds
	ret.fdstart = 3
	ret.cwd = cwd
	if ret.cwd.fops.reopen() != 0 {
		panic("must succeed")
	}
	ret.mmapi = USERMIN
	ret.ulim = _deflimits

	ret.threadi.init()
	ret.tid0 = ret.tid_new()

	ret.mywait.wait_init()
	ret.mywait.start_thread(ret.tid0)

	return ret
}

func proc_get(pid int) *proc_t {
	proclock.Lock()
	p, ok := allprocs[pid]
	proclock.Unlock()
	if !ok {
		panic(fmt.Sprintf("no such pid %d", pid))
	}
	return p
}

func proc_check(pid int) (*proc_t, bool) {
	proclock.Lock()
	p, ok := allprocs[pid]
	proclock.Unlock()
	return p, ok
}

func proc_del(pid int) {
	proclock.Lock()
	_, ok := allprocs[pid]
	if !ok {
		panic("bad pid")
	}
	delete(allprocs, pid)
	proclock.Unlock()
}

// prepare to write to the user page that contains userva that may be marked
// COW.  caller must hold pmap lock (and the copy must take place under the
// same lock acquisition). this can go away once we can handle syscalls with
// the user pmap still loaded.
func (p *proc_t) cowfault(userva int) {
	// userva is not guaranteed to be valid
	if userva < USERMIN {
		return
	}
	p.lockassert_pmap()
	pte := pmap_walk(p.pmap, userva, PTE_U | PTE_W)
	if *pte & PTE_P != 0 && *pte & PTE_COW == 0 {
		return
	}
	vmi, ok := p.vmregion.lookup(uintptr(userva))
	if !ok {
		return
	}
	ecode := uintptr(PTE_U | PTE_W)
	sys_pgfault(p, vmi, pte, uintptr(userva), ecode)
}

// an fd table invariant: every fd must have its file field set. thus the
// caller cannot set an fd's file field without holding fdl. otherwise you will
// race with a forking thread when it copies the fd table.
func (p *proc_t) fd_insert(f *fd_t, perms int) int {
	p.fdl.Lock()

	// find free fd
	newfd := p.fdstart
	found := false
	for newfd < len(p.fds) {
		if p.fds[newfd] == nil {
			p.fdstart = newfd + 1
			found = true
			break
		}
		newfd++
	}
	if !found {
		// double size of fd table
		ol := len(p.fds)
		nfdt := make([]*fd_t, 2*ol)
		copy(nfdt, p.fds)
		p.fds = nfdt
	}
	fdn := newfd
	//fd := &fd_t{}
	fd := f
	fd.perms = perms
	if p.fds[fdn] != nil {
		panic(fmt.Sprintf("new fd exists %d", fdn))
	}
	p.fds[fdn] = fd
	if fd.fops == nil {
		panic("wtf!")
	}
	p.fdl.Unlock()
	return fdn
}

// fdn is not guaranteed to be a sane fd
func (p *proc_t) fd_get_inner(fdn int) (*fd_t, bool) {
	if fdn < 0 || fdn >= len(p.fds) {
		return nil, false
	}
	ret := p.fds[fdn]
	ok := ret != nil
	return ret, ok
}

func (p *proc_t) fd_get(fdn int) (*fd_t, bool) {
	p.fdl.Lock()
	ret, ok := p.fd_get_inner(fdn)
	p.fdl.Unlock()
	return ret, ok
}

// fdn is not guaranteed to be a sane fd
func (p *proc_t) fd_del(fdn int) (*fd_t, bool) {
	p.fdl.Lock()

	if fdn < 0 || fdn >= len(p.fds) {
		p.fdl.Unlock()
		return nil, false
	}
	ret := p.fds[fdn]
	p.fds[fdn] = nil
	ok := ret != nil
	if ok && fdn < p.fdstart {
		p.fdstart = fdn
	}
	p.fdl.Unlock()
	return ret, ok
}

func (parent *proc_t) vm_fork(child *proc_t, rsp int) bool {
	// first add kernel pml4 entries
	for _, e := range kents {
		child.pmap[e.pml4slot] = e.entry
	}
	// recursive mapping
	child.pmap[VREC] = child.p_pmap | PTE_P | PTE_W

	doflush := false
	child.vmregion = parent.vmregion.copy()
	parent.vmregion.iter(func(vmi *vminfo_t) {
		start := int(vmi.pgn << PGSHIFT)
		end := start + int(vmi.pglen << PGSHIFT)
		//fmt.Printf("fork reg: %x %x\n", start, end)
		if ptefork(child.pmap, parent.pmap, start, end) {
			doflush = true
		}
	})

	// don't mark stack COW since the parent/child will fault their stacks
	// immediately
	pte := pmap_lookup(child.pmap, rsp)
	// give up if we can't find the stack
	if pte == nil || *pte & PTE_P == 0 || *pte & PTE_U == 0 {
		return doflush
	}
	// sys_pgfault expects pmap to be locked
	child.Lock_pmap()
	vmi, ok := child.vmregion.lookup(uintptr(rsp))
	if !ok {
		panic("must be mapped")
	}
	sys_pgfault(child, vmi, pte, uintptr(rsp), uintptr(PTE_U | PTE_W))
	child.Unlock_pmap()
	pte = pmap_lookup(parent.pmap, rsp)
	if pte == nil || *pte & PTE_P == 0 || *pte & PTE_U == 0 {
		panic("child has stack but not parent")
	}
	*pte &^= PTE_COW
	*pte |= PTE_W | PTE_WASCOW

	return doflush
}

// does not increase opencount on fops (vmadd_file does). perms should only use
// PTE_U/PTE_W; the page fault handler will install the correct COW flags.
// perms == 0 means that no mapping can go here (like for guard pages).
func (p *proc_t) _mkvmi(mt mtype_t, start, len, perms, foff int,
    fops fdops_i) *vminfo_t {
	if (start | len) & PGOFFSET != 0 {
		//fmt.Printf("%x %x\n", start, len)
		panic("start and len must be aligned")
	}
	// don't specify cow, present etc. -- page fault will handle all that
	pm := PTE_W | PTE_COW | PTE_WASCOW | PTE_PS | PTE_PCD | PTE_P | PTE_U
	if r := perms & pm; r != 0 && r != PTE_U && r != (PTE_W | PTE_U) {
		panic("bad perms")
	}
	ret := &vminfo_t{}
	pgn := uintptr(start) >> PGSHIFT
	pglen := roundup(len, PGSIZE) >> PGSHIFT
	ret.mtype = mt
	ret.pgn = pgn
	ret.pglen = pglen
	ret.perms = uint(perms)
	if mt == VFILE {
		ret.file.foff = foff
		ret.file.mfile = &mfile_t{}
		ret.file.mfile.mfops = fops
		ret.file.mfile.mapcount = pglen
	}
	return ret
}

func (p *proc_t) vmadd_anon(start, len, perms int) {
	vmi := p._mkvmi(VANON, start, len, perms, 0, nil)
	p.vmregion.insert(vmi)
}

func (p *proc_t) vmadd_file(start, len, perms int, fops fdops_i, foff int) {
	vmi := p._mkvmi(VFILE, start, len, perms, foff, fops)
	p.vmregion.insert(vmi)
}

func (p *proc_t) mkuserbuf(userva, len int) *userbuf_t {
	ret := &userbuf_t{}
	ret.ub_init(p, userva, len)
	return ret
}

var ubpool = sync.Pool{New: func() interface{} { return new(userbuf_t) }}

func (p *proc_t) mkuserbuf_pool(userva, len int) *userbuf_t {
	ret := ubpool.Get().(*userbuf_t)
	ret.ub_init(p, userva, len)
	return ret
}

func (p *proc_t) mkfxbuf() *[64]int {
	ret := new([64]int)
	n := uintptr(unsafe.Pointer(ret))
	if n & ((1 << 4) - 1) != 0 {
		panic("not 16 byte aligned")
	}
	return ret
}

func (p *proc_t) page_insert(va int, p_pg int, perms int, vempty bool) {
	p.lockassert_pmap()
	refup(uintptr(p_pg))
	pte := pmap_walk(p.pmap, va, PTE_U | PTE_W)
	ninval := false
	var p_old uintptr
	if pte != nil && *pte & PTE_P != 0 {
		if vempty {
			panic("pte not empty")
		}
		ninval = true
		p_old = uintptr(*pte & PTE_ADDR)
	}
	*pte = p_pg | perms | PTE_P
	if ninval {
		invlpg(va)
		refdown(p_old)
	}
}

func (p *proc_t) page_remove(va int) (uintptr, bool) {
	p.lockassert_pmap()
	remmed := false
	pte := pmap_lookup(p.pmap, va)
	var p_old uintptr
	if pte != nil && *pte & PTE_P != 0 {
		p_old = uintptr(*pte & PTE_ADDR)
		*pte = 0
		invlpg(va)
		remmed = true
	}
	return p_old, remmed
}

func (p *proc_t) pgfault(tid tid_t, fa, ecode uintptr) bool {
	p.Lock_pmap()
	vmi, ok := p.vmregion.lookup(fa)
	if ok {
		iswrite := ecode & uintptr(PTE_W) != 0
		writeok := vmi.perms & uint(PTE_W) != 0
		if iswrite && !writeok {
			ok = false
		}
	}
	if !ok {
		p.Unlock_pmap()
		return false
	}
	sys_pgfault(p, vmi, nil, fa, ecode)
	p.Unlock_pmap()
	return true
}

func (p *proc_t) tlbshoot(startva, pgcount int) {
	if pgcount == 0 {
		return
	}
	p.lockassert_pmap()
	if p.thread_count() > 1 {
		tlb_shootdown(p.p_pmap, startva, pgcount)
	}
}

func (p *proc_t) resched(tid tid_t) bool {
	p.threadi.Lock()
	talive := p.threadi.alive[tid]
	p.threadi.Unlock()
	if talive && p.doomed {
		// although this thread is still alive, the process should
		// terminate
		reap_doomed(p, tid)
		return false
	}
	return talive
}

func (p *proc_t) run(tf *[TFSIZE]int, tid tid_t) {
	fastret := false
	// could allocate fxbuf lazily
	fxbuf := p.mkfxbuf()
	for p.resched(tid) {
		// for fast syscalls, we restore little state. thus we must
		// distinguish between returning to the user program after it
		// was interrupted by a timer interrupt/CPU exception vs a
		// syscall.
		refp, _ := _refaddr(uintptr(p.p_pmap))
		intno, aux, op_pmap, odec := runtime.Userrun(tf, fxbuf, p.pmap,
		    uintptr(p.p_pmap), fastret, refp)
		fastret = false
		switch intno {
		case SYSCALL:
			// fast return doesn't restore the registers used to
			// specify the arguments for libc _entry(), so do a
			// slow return when returning from sys_execv().
			sysno := tf[TF_RAX]
			if sysno != SYS_EXECV {
				fastret = true
			}
			tf[TF_RAX] = syscall(p, tid, tf)
		case TIMER:
			//fmt.Printf(".")
			runtime.Gosched()
		case PGFAULT:
			faultaddr := uintptr(aux)
			if !p.pgfault(tid, faultaddr, uintptr(tf[TF_ERROR])) {
				fmt.Printf("*** fault *** %v: addr %x, " +
				    "rip %x. killing...\n", p.name, faultaddr,
				    tf[TF_RIP])
				sys_exit(p, tid, SIGNALED | mkexitsig(11))
			}
		case DIVZERO, GPFAULT, UD:
			fmt.Printf("%s -- TRAP: %v, RIP: %x\n", p.name, intno,
			    tf[TF_RIP])
			sys_exit(p, tid, SIGNALED | mkexitsig(4))
		case TLBSHOOT, INT_KBD, INT_COM1, INT_DISK:
			// XXX: shouldn't interrupt user program execution...
		default:
			panic(fmt.Sprintf("weird trap: %d", intno))
		}
		// did we switch pmaps? if so, the old pmap may need to be
		// freed.
		if odec {
			dec_pmap(op_pmap)
		}
	}
}

func (p *proc_t) sched_add(tf *[TFSIZE]int, tid tid_t) {
	go p.run(tf, tid)
}

func (p *proc_t) tid_new() tid_t {
	ret := tid_t(newpid())

	p.threadi.Lock()
	p.threadi.alive[ret] = true
	p.threadi.Unlock()

	return ret
}

func (p *proc_t) thread_count() int {
	p.threadi.Lock()
	ret := len(p.threadi.alive)
	p.threadi.Unlock()
	return ret
}

// remove a particular thread that was never added to scheduler; like when fork
// fails.
func (p *proc_t) thread_del(tid tid_t) {
	p.threadi.Lock()
	ti := &p.threadi
	delete(ti.alive, tid)
	p.threadi.Unlock()
}

// terminate a single thread
func (p *proc_t) thread_dead(tid tid_t, status int, usestatus bool) {
	// XXX exit process if thread is thread0, even if other threads exist
	p.threadi.Lock()
	ti := &p.threadi
	delete(ti.alive, tid)
	destroy := len(ti.alive) == 0

	if usestatus {
		p.exitstatus = status
	}
	p.threadi.Unlock()

	// update rusage user time
	//utime := runtime.Proctime(p.mkptid(tid))
	//if utime < 0 {
	//	panic("tid must exist")
	//}
	utime := 42
	p.atime.utadd(utime)

	// put thread status in this process's wait info; threads don't have
	// rusage for now.
	p.mywait.put(int(tid), status, nil)

	if destroy {
		p.terminate()
	}
}

func (p *proc_t) doomall() {
	p.doomed = true
}

func _scanadd(pg *[512]int, depth int, l *[]int) {
	if depth == 0 {
		return
	}
	for _, pte := range pg {
		if pte & PTE_P == 0 || pte & PTE_U == 0 || pte & PTE_PS != 0 {
			continue
		}
		phys := pte & PTE_ADDR
		*l = append(*l, phys)
		_scanadd(dmap(phys), depth - 1, l)
	}
}

var fpool = sync.Pool{New: func() interface{} { return make([]int, 0, 10) }}

// walks the pmap, adding each page to a slice to be freed
func _vmfree(p_pmap uintptr) int {
	//pgs := make([]int, 0, 10)
	// XXX try putting pgs on stack; does escape analysis still screw up?
	pgs := fpool.Get().([]int)
	_scanadd(dmap(int(p_pmap)), 4, &pgs)
	// XXX this takes the free list lock many times
	for _, p_pg := range pgs {
		refdown(uintptr(p_pg))
	}
	fpool.Put(pgs[0:0])
	return len(pgs)
}

func dec_pmap(p_pmap uintptr) {
	freed, idx := _refdown(p_pmap)
	if !freed {
		return
	}
	_vmfree(p_pmap)
	_reffree(idx)
}

// termiante a process. must only be called when the process has no more
// running threads.
func (p *proc_t) terminate() {
	if p.pid == 1 {
		panic("killed init")
	}

	p.threadi.Lock()
	ti := &p.threadi
	if len(ti.alive) != 0 {
		panic("terminate, but threads alive")
	}
	p.threadi.Unlock()

	// close open fds
	p.fdl.Lock()
	for i := range p.fds {
		if p.fds[i] == nil {
			continue
		}
		close_panic(p.fds[i])
	}
	p.fdl.Unlock()
	close_panic(p.cwd)

	proc_del(p.pid)
	dec_pmap(uintptr(p.p_pmap))

	// send status to parent
	if p.pwait == nil {
		panic("nil pwait")
	}

	// combine total child rusage with ours, send to parent
	na := accnt_t{userns: p.atime.userns, sysns: p.atime.sysns}
	// calling na.add() makes the compiler allocate na in the heap! escape
	// analysis' fault?
	//na.add(&p.catime)
	na.userns += p.catime.userns
	na.sysns += p.catime.sysns

	// put process exit status to parent's wait info
	p.pwait.put(p.pid, p.exitstatus, &na)
}

func (p *proc_t) Lock_pmap() {
	// useful for finding deadlock bugs with one cpu
	//if p.pgfltaken {
	//	panic("double lock")
	//}
	p.pgfl.Lock()
	p.pgfltaken = true
}

func (p *proc_t) Unlock_pmap() {
	p.pgfltaken = false
	p.pgfl.Unlock()
}

func (p *proc_t) lockassert_pmap() {
	if !p.pgfltaken {
		panic("pgfl lock must be held")
	}
}

// returns a slice whose underlying buffer points to va, which can be
// page-unaligned. the length of the returned slice is (PGSIZE - (va % PGSIZE))
// k2u is true if kernel memory is the source and user memory is the
// destination of the copy, otherwise the opposite.
func (p *proc_t) userdmap8_inner(va int, k2u bool) ([]uint8, bool) {
	p.lockassert_pmap()
	if va < USERMIN {
		return nil, false
	}
	if k2u {
		p.cowfault(va)
	}
	voff := va & PGOFFSET
	pte := pmap_lookup(p.pmap, va)
	if pte == nil || *pte & PTE_P == 0 {
		// if user memory is the source, it is possible that the page
		// just hasn't been faulted in yet. try to fault in the page
		// before failing.
		uva := uintptr(va)
		if vmi, ok := p.vmregion.lookup(uva); ok {
			ecode := uintptr(PTE_U)
			sys_pgfault(p, vmi, pte, uva, ecode)
		} else {
			return nil, false
		}
	}
	if *pte & PTE_U == 0 {
		return nil, false
	}
	pg := dmap(*pte & PTE_ADDR)
	bpg := (*[PGSIZE]uint8)(unsafe.Pointer(pg))
	return bpg[voff:], true
}

func (p *proc_t) _userdmap8(va int, k2u bool) ([]uint8, bool) {
	p.Lock_pmap()
	ret, ok := p.userdmap8_inner(va, k2u)
	p.Unlock_pmap()
	return ret, ok
}

func (p *proc_t) userdmap8r(va int) ([]uint8, bool) {
	return p._userdmap8(va, false)
}

func (p *proc_t) userdmap8w(va int) ([]uint8, bool) {
	return p._userdmap8(va, true)
}

func (p *proc_t) usermapped(va, n int) bool {
	p.Lock_pmap()
	defer p.Unlock_pmap()

	_, ok := p.vmregion.lookup(uintptr(va))
	return ok
}

func (p *proc_t) userreadn(va, n int) (int, bool) {
	if n > 8 {
		panic("large n")
	}
	p.Lock_pmap()
	defer p.Unlock_pmap()
	var ret int
	var src []uint8
	var ok bool
	for i := 0; i < n; i += len(src) {
		src, ok = p.userdmap8_inner(va + i, false)
		if !ok {
			return 0, false
		}
		l := n - i
		if len(src) < l {
			l = len(src)
		}
		v := readn(src, l, 0)
		ret |= v << (8*uint(i))
	}
	return ret, true
}

func (p *proc_t) userwriten(va, n, val int) bool {
	if n > 8 {
		panic("large n")
	}
	p.Lock_pmap()
	defer p.Unlock_pmap()
	var dst []uint8
	for i := 0; i < n; i += len(dst) {
		v := val >> (8*uint(i))
		t, ok := p.userdmap8_inner(va + i, true)
		dst = t
		if !ok {
			return false
		}
		writen(dst, n - i, 0, v)
	}
	return true
}

// first ret value is the string from user space
// second ret value is whether or not the string is mapped
// third ret value is whether the string length is less than lenmax
func (p *proc_t) userstr(uva int, lenmax int) (string, bool, bool) {
	if lenmax < 0 {
		return "", false, false
	}
	p.Lock_pmap()
	i := 0
	var s string
	for {
		str, ok := p.userdmap8_inner(uva + i, false)
		if !ok {
			p.Unlock_pmap()
			return "", false, false
		}
		for j, c := range str {
			if c == 0 {
				s = s + string(str[:j])
				p.Unlock_pmap()
				return s, true, false
			}
		}
		s = s + string(str)
		i += len(str)
		if len(s) >= lenmax {
			p.Unlock_pmap()
			return "", true, true
		}
	}
}

func (p *proc_t) usertimespec(va int) (time.Duration, time.Time, int) {
	secs, ok1 := p.userreadn(va, 8)
	nsecs, ok2 := p.userreadn(va + 8, 8)
	var zt time.Time
	if !ok1 || !ok2 {
		return 0, zt, -EFAULT
	}
	if secs < 0 || nsecs < 0 {
		return 0, zt, -EINVAL
	}
	tot := time.Duration(secs) * time.Second
	tot += time.Duration(nsecs) * time.Nanosecond
	t := time.Unix(int64(secs), int64(nsecs))
	return tot, t, 0
}

func (p *proc_t) userargs(uva int) ([]string, bool) {
	if uva == 0 {
		return nil, true
	}
	isnull := func(cptr []uint8) bool {
		for _, b := range cptr {
			if b != 0 {
				return false
			}
		}
		return true
	}
	ret := make([]string, 0)
	argmax := 64
	addarg := func(cptr []uint8) bool {
		if len(ret) > argmax {
			return false
		}
		var uva int
		// cptr is little-endian
		for i, b := range cptr {
			uva = uva | int(uint(b)) << uint(i*8)
		}
		lenmax := 128
		str, ok, long := p.userstr(uva, lenmax)
		if !ok || long {
			return false
		}
		ret = append(ret, str)
		return true
	}
	uoff := 0
	psz := 8
	done := false
	curaddr := make([]uint8, 0, 8)
	for !done {
		ptrs, ok := p.userdmap8r(uva + uoff)
		if !ok {
			return nil, false
		}
		for _, ab := range ptrs {
			curaddr = append(curaddr, ab)
			if len(curaddr) == psz {
				if isnull(curaddr) {
					done = true
					break
				}
				if !addarg(curaddr) {
					return nil, false
				}
				curaddr = curaddr[0:0]
			}
		}
		uoff += len(ptrs)
	}
	return ret, true
}

// copies src to the user virtual address uva. may copy part of src if uva +
// len(src) is not mapped
func (p *proc_t) k2user(src []uint8, uva int) bool {
	p.Lock_pmap()
	ret := p.k2user_inner(src, uva)
	p.Unlock_pmap()
	return ret
}

func (p *proc_t) k2user_inner(src []uint8, uva int) bool {
	p.lockassert_pmap()
	cnt := 0
	l := len(src)
	for cnt != l {
		dst, ok := p.userdmap8_inner(uva + cnt, true)
		if !ok {
			return false
		}
		ub := len(src)
		if ub > len(dst) {
			ub = len(dst)
		}
		copy(dst, src)
		src = src[ub:]
		cnt += ub
	}
	return true
}

// copies len(dst) bytes from userspace address uva to dst
func (p *proc_t) user2k(dst []uint8, uva int) bool {
	p.Lock_pmap()
	ret := p.user2k_inner(dst, uva)
	p.Unlock_pmap()
	return ret
}

func (p *proc_t) user2k_inner(dst []uint8, uva int) bool {
	p.lockassert_pmap()
	cnt := 0
	l := len(dst)
	for cnt != l {
		src, ok := p.userdmap8_inner(uva + cnt, false)
		if !ok {
			return false
		}
		ub := len(dst)
		if ub > len(src) {
			ub = len(src)
		}
		copy(dst, src)
		dst = dst[ub:]
		cnt += ub
	}
	return true
}

func (p *proc_t) unusedva_inner(startva, len int) int {
	p.lockassert_pmap()
	if len < 0 || len > 1 << 48 {
		panic("weird len")
	}
	startva = rounddown(startva, PGSIZE)
	if startva < USERMIN {
		startva = USERMIN
	}
	_ret, _l := p.vmregion.empty(uintptr(startva), uintptr(len))
	ret := int(_ret)
	l := int(_l)
	if startva > ret && startva < ret + l {
		ret = startva
	}
	return ret
}

// a helper object for read/writing from userspace memory. virtual address
// lookups and reads/writes to those addresses must be atomic with respect to
// page faults.
type userbuf_t struct {
	userva	int
	len	int
	// 0 <= off <= len
	off	int
	proc	*proc_t
	// "fake" is a hack for easy reading/writing kernel memory only (like
	// fetching the ELF header from the pagecache during exec)
	// XXX
	fake	bool
	fbuf	[]uint8
}

func (ub *userbuf_t) ub_init(p *proc_t, uva, len int) {
	// XXX fix signedness
	if len < 0 {
		panic("negative length")
	}
	if len >= 1 << 39 {
		fmt.Printf("suspiciously large user buffer\n")
	}
	ub.userva = uva
	ub.len = len
	ub.off = 0
	ub.proc = p
}

func (ub *userbuf_t) fake_init(buf []uint8) {
	ub.fake = true
	ub.fbuf = buf
	ub.off = 0
}

func (ub *userbuf_t) remain() int {
	if ub.fake {
		return len(ub.fbuf)
	}
	return ub.len - ub.off
}

func (ub *userbuf_t) read(dst []uint8) (int, int) {
	return ub._tx(dst, false)
}

func (ub *userbuf_t) write(src []uint8) (int, int) {
	return ub._tx(src, true)
}

// copies the min of either the provided buffer or ub.len. returns number of
// bytes copied and error.
func (ub *userbuf_t) _tx(buf []uint8, write bool) (int, int) {
	if ub.fake {
		var c int
		if write {
			c = copy(ub.fbuf, buf)
		} else {
			c = copy(buf, ub.fbuf)
		}
		ub.fbuf = ub.fbuf[c:]
		return c, 0
	}

	// serialize with page faults
	ub.proc.Lock_pmap()

	ret := 0
	for len(buf) != 0 && ub.off != ub.len {
		va := ub.userva + ub.off
		ubuf, ok := ub.proc.userdmap8_inner(va, write)
		if !ok {
			ub.proc.Unlock_pmap()
			return ret, -EFAULT
		}
		end := ub.off + len(ubuf)
		if end > ub.len {
			left := ub.len - ub.off
			ubuf = ubuf[:left]
		}
		var c int
		if write {
			c = copy(ubuf, buf)
		} else {
			c = copy(buf, ubuf)
		}
		buf = buf[c:]
		ub.off += c
		ret += c
	}
	ub.proc.Unlock_pmap()
	return ret, 0
}

// a circular buffer that is read/written by userspace. not thread-safe -- it
// is intended to be used by one daemon.
type circbuf_t struct {
	buf	[]uint8
	bufsz	int
	head	int
	tail	int
}

var _bufpool = sync.Pool{New: func() interface{} { return make([]uint8, 512)}}

func (cb *circbuf_t) cb_init(sz int) {
	bufmax := 1024*1024
	if sz < 0 || sz > bufmax {
		panic("bad circbuf size")
	}
	cb.bufsz = sz
	cb.buf = _bufpool.Get().([]uint8)
	if len(cb.buf) < sz {
		cb.buf = make([]uint8, cb.bufsz)
	}
	cb.head, cb.tail = 0, 0
}

func (cb *circbuf_t) cb_release() {
	_bufpool.Put(cb.buf)
	cb.buf = nil
}

func (cb *circbuf_t) full() bool {
	return cb.head - cb.tail == cb.bufsz
}

func (cb *circbuf_t) empty() bool {
	return cb.head == cb.tail
}

func (cb *circbuf_t) copyin(src *userbuf_t) (int, int) {
	if cb.full() {
		panic("cb.buf full; should have blocked")
	}
	hi := cb.head % cb.bufsz
	ti := cb.tail % cb.bufsz
	c := 0
	// wraparound?
	if ti <= hi {
		dst := cb.buf[hi:]
		wrote, err := src.read(dst)
		if err != 0 {
			return 0, err
		}
		if wrote != len(dst) {
			cb.head += wrote
			return wrote, 0
		}
		c += wrote
		hi = (cb.head + wrote) % cb.bufsz
	}
	// XXXPANIC
	if hi > ti {
		panic("wut?")
	}
	dst := cb.buf[hi:ti]
	wrote, err := src.read(dst)
	if err != 0 {
		return 0, err
	}
	c += wrote
	cb.head += c
	return c, 0
}

func (cb *circbuf_t) copyout(dst *userbuf_t) (int, int) {
	if cb.empty() {
		return 0, 0
	}
	hi := cb.head % cb.bufsz
	ti := cb.tail % cb.bufsz
	c := 0
	// wraparound?
	if hi <= ti {
		src := cb.buf[ti:]
		wrote, err := dst.write(src)
		if err != 0 {
			return 0, err
		}
		if wrote != len(src) {
			cb.tail += wrote
			return wrote, 0
		}
		c += wrote
		ti = (cb.tail + wrote) % cb.bufsz
	}
	// XXXPANIC
	if ti > hi {
		panic("wut?")
	}
	src := cb.buf[ti:hi]
	wrote, err := dst.write(src)
	if err != 0 {
		return 0, err
	}
	c += wrote
	cb.tail += c
	return c, 0
}

func cpus_stack_init(apcnt int, stackstart uintptr) {
	for i := 0; i < apcnt; i++ {
		// allocate/map interrupt stack
		kmalloc(stackstart, PTE_W)
		stackstart += PGSIZEW
		assert_no_va_map(kpmap(), stackstart)
		stackstart += PGSIZEW
		// allocate/map NMI stack
		kmalloc(stackstart, PTE_W)
		stackstart += PGSIZEW
		assert_no_va_map(kpmap(), stackstart)
		stackstart += PGSIZEW
	}
}

func cpus_start(ncpu, aplim int) {
	runtime.GOMAXPROCS(1 + aplim)
	apcnt := ncpu - 1

	fmt.Printf("found %v CPUs\n", ncpu)

	if apcnt == 0 {
		fmt.Printf("uniprocessor\n")
		return
	}

	// AP code must be between 0-1MB because the APs are in real mode. load
	// code to 0x8000 (overwriting bootloader)
	mpaddr := 0x8000
	mpcode := allbins["mpentry.bin"].data
	c := 0
	mpl := len(mpcode)
	for c < mpl {
		mppg := dmap8(mpaddr + c)
		did := copy(mppg, mpcode)
		mpcode = mpcode[did:]
		c += did
	}

	// skip mucking with CMOS reset code/warm reset vector (as per the the
	// "universal startup algoirthm") and instead use the STARTUP IPI which
	// is supported by lapics of version >= 1.x. (the runtime panicks if a
	// lapic whose version is < 1.x is found, thus assume their absence).
	// however, only one STARTUP IPI is accepted after a CPUs RESET or INIT
	// pin is asserted, thus we need to send an INIT IPI assert first (it
	// appears someone already used a STARTUP IPI; probably the BIOS).

	lapaddr := 0xfee00000
	pte := pmap_lookup(kpmap(), lapaddr)
	if pte == nil || *pte & PTE_P == 0 || *pte & PTE_PCD == 0 {
		panic("lapaddr unmapped")
	}
	lap := (*[PGSIZE/4]uint32)(unsafe.Pointer(uintptr(lapaddr)))
	icrh := 0x310/4
	icrl := 0x300/4

	ipilow := func(ds int, t int, l int, deliv int, vec int) uint32 {
		return uint32(ds << 18 | t << 15 | l << 14 |
		    deliv << 8 | vec)
	}

	icrw := func(hi uint32, low uint32) {
		// use sync to guarantee order
		atomic.StoreUint32(&lap[icrh], hi)
		atomic.StoreUint32(&lap[icrl], low)
		ipisent := uint32(1 << 12)
		for atomic.LoadUint32(&lap[icrl]) & ipisent != 0 {
		}
	}

	// destination shorthands:
	// 1: self
	// 2: all
	// 3: all but me

	initipi := func(assert bool) {
		vec := 0
		delivmode := 0x5
		level := 1
		trig  := 0
		dshort:= 3
		if !assert {
			trig = 1
			level = 0
			dshort = 2
		}
		hi  := uint32(0)
		low := ipilow(dshort, trig, level, delivmode, vec)
		icrw(hi, low)
	}

	startupipi := func() {
		vec       := mpaddr >> 12
		delivmode := 0x6
		level     := 0x1
		trig      := 0x0
		dshort    := 0x3

		hi := uint32(0)
		low := ipilow(dshort, trig, level, delivmode, vec)
		icrw(hi, low)
	}

	// pass arguments to the ap startup code via secret storage (the old
	// boot loader page at 0x7c00)

	// secret storage layout
	// 0 - e820map
	// 1 - pmap
	// 2 - firstfree
	// 3 - ap entry
	// 4 - gdt
	// 5 - gdt
	// 6 - idt
	// 7 - idt
	// 8 - ap count
	// 9 - stack start
	// 10- proceed

	ss := (*[11]uintptr)(unsafe.Pointer(uintptr(0x7c00)))
	sap_entry := 3
	sgdt      := 4
	sidt      := 6
	sapcnt    := 8
	sstacks   := 9
	sproceed  := 10
	var _dur func(uint)
	_dur = ap_entry
	ss[sap_entry] = **(**uintptr)(unsafe.Pointer(&_dur))
	// sgdt and sidt save 10 bytes
	runtime.Sgdt(&ss[sgdt])
	runtime.Sidt(&ss[sidt])
	ss[sapcnt] = 0
	// for BSP:
	// 	int stack	[0xa100000000, 0xa100001000)
	// 	guard page	[0xa100001000, 0xa100002000)
	// 	NMI stack	[0xa100002000, 0xa100003000)
	// 	guard page	[0xa100003000, 0xa100004000)
	// for each AP:
	// 	int stack	BSP's + apnum*4*PGSIZE + 0*PGSIZE
	// 	NMI stack	BSP's + apnum*4*PGSIZE + 2*PGSIZE
	stackstart := uintptr(0xa100004000)
	ss[sstacks] = stackstart   // each ap grabs a unique stack
	ss[sproceed] = 0

	// XXX make sure secret storage values are not in store buffer
	dummy := int64(0)
	atomic.CompareAndSwapInt64(&dummy, 0, 10)

	initipi(true)
	// not necessary since we assume lapic version >= 1.x (ie not 82489DX)
	//initipi(false)
	cdelay(1)

	startupipi()
	cdelay(1)
	startupipi()

	// wait a while for hopefully all APs to join. it'd be better to use
	// ACPI to determine the correct count of CPUs and then wait for them
	// all to join.
	cdelay(500)
	apcnt = int(ss[sapcnt])
	if apcnt > aplim {
		apcnt = aplim
	}
	set_cpucount(apcnt + 1)

	// actually map the stacks for the CPUs that joined
	cpus_stack_init(apcnt, stackstart)

	// tell the cpus to carry on
	ss[sproceed] = uintptr(apcnt)

	fmt.Printf("done! %v APs found (%v joined)\n", ss[sapcnt], apcnt)
}

// myid is a logical id, not lapic id
//go:nosplit
func ap_entry(myid uint) {

	// myid starts from 1
	runtime.Ap_setup(myid)

	lid := lap_id()
	if lid > maxcpus || lid < 0 {
		runtime.Pnum(0xb1dd1e)
		for {}
	}
	cpus[lid].num = int(myid)

	// ints are still cleared. wait for timer int to enter scheduler
	fl := runtime.Pushcli()
	fl |= TF_FL_IF
	runtime.Popcli(fl)
	for {}
}

func set_cpucount(n int) {
	numcpus = n
}

func irq_unmask(irq int) {
	apic.irq_unmask(irq)
}

func irq_eoi(irq int) {
	//apic.eoi(irq)
	apic.irq_unmask(irq)
}

func kbd_init() {
	km := make(map[int]byte)
	NO := byte(0)
	tm := []byte{
	    // ty xv6
	    NO,   0x1B, '1',  '2',  '3',  '4',  '5',  '6',  // 0x00
	    '7',  '8',  '9',  '0',  '-',  '=',  '\b', '\t',
	    'q',  'w',  'e',  'r',  't',  'y',  'u',  'i',  // 0x10
	    'o',  'p',  '[',  ']',  '\n', NO,   'a',  's',
	    'd',  'f',  'g',  'h',  'j',  'k',  'l',  ';',  // 0x20
	    '\'', '`',  NO,   '\\', 'z',  'x',  'c',  'v',
	    'b',  'n',  'm',  ',',  '.',  '/',  NO,   '*',  // 0x30
	    NO,   ' ',  NO,   NO,   NO,   NO,   NO,   NO,
	    NO,   NO,   NO,   NO,   NO,   NO,   NO,   '7',  // 0x40
	    '8',  '9',  '-',  '4',  '5',  '6',  '+',  '1',
	    '2',  '3',  '0',  '.',  NO,   NO,   NO,   NO,   // 0x50
	    }

	for i, c := range tm {
		if c != NO {
			km[i] = c
		}
	}
	cons.kbd_int = make(chan bool)
	cons.com_int = make(chan bool)
	cons.reader = make(chan []byte)
	cons.reqc = make(chan int)
	go kbd_daemon(&cons, km)
	irq_unmask(IRQ_KBD)
	irq_unmask(IRQ_COM1)

	// make sure kbd int and com1 int are clear
	for _kready() {
		runtime.Inb(0x60)
	}
	for _comready() {
		runtime.Inb(0x3f8)
	}
}

type cons_t struct {
	kbd_int		chan bool
	com_int		chan bool
	reader		chan []byte
	reqc		chan int
}

var cons	= cons_t{}

func _comready() bool {
	com1ctl := uint16(0x3f8 + 5)
	b := runtime.Inb(com1ctl)
	if b & 0x01 == 0 {
		return false
	}
	return true
}
func _kready() bool {
	ibf := uint(1 << 0)
	st := runtime.Inb(0x64)
	if st & ibf == 0 {
		//panic("no kbd data?")
		return false
	}
	return true
}

var _nflip int

func kbd_daemon(cons *cons_t, km map[int]byte) {
	inb := runtime.Inb
	start := make([]byte, 0, 10)
	data := start
	addprint := func(c byte) {
		fmt.Printf("%c", c)
		data = append(data, c)
		if c == '\\' {
			apic.dump()
			debug.SetTraceback("all")
			panic("yahoo")
		} else if c == '@' {
			_nflip = (_nflip + 1) % 2
			act := PROF_GOLANG
			if _nflip == 0 {
				act |= PROF_DISABLE
			}
			sys_prof(nil, act, 0, 0, 0)
		}
	}
	var reqc chan int
	for {
		select {
		case <- cons.kbd_int:
			for _kready() {
				sc := int(inb(0x60))
				c, ok := km[sc]
				if ok {
					addprint(c)
				}
			}
			irq_eoi(IRQ_KBD)
		case <- cons.com_int:
			for _comready() {
				com1data := uint16(0x3f8 + 0)
				sc := inb(com1data)
				c := byte(sc)
				if c == '\r' {
					c = '\n'
				} else if c == 127 {
					// delete -> backspace
					c = '\b'
				}
				addprint(c)
			}
			irq_eoi(IRQ_COM1)
		case l := <- reqc:
			if l > len(data) {
				l = len(data)
			}
			s := data[0:l]
			cons.reader <- s
			data = data[l:]
		}
		if len(data) == 0 {
			reqc = nil
			data = start
		} else {
			reqc = cons.reqc
		}
	}
}

// reads keyboard data, blocking for at least 1 byte. returns at most cnt
// bytes.
func kbd_get(cnt int) []byte {
	if cnt < 0 {
		panic("negative cnt")
	}
	cons.reqc <- cnt
	return <- cons.reader
}

func attach_devs() int {
	ncpu := acpi_attach()
	pcibus_attach()
	return ncpu
}

func tlb_shootdown(p_pmap, va, pgcount int) {
	if numcpus == 1 {
		return
	}
	othercpus := uintptr(numcpus - 1)
	mygen := runtime.Tlbadmit(uintptr(p_pmap), othercpus, uintptr(va),
	    uintptr(pgcount))

	lapaddr := 0xfee00000
	lap := (*[PGSIZE/4]uint32)(unsafe.Pointer(uintptr(lapaddr)))
	ipilow := func(ds int, deliv int, vec int) uint32 {
		return uint32(ds << 18 | 1 << 14 | deliv << 8 | vec)
	}

	icrw := func(hi uint32, low uint32) {
		icrh := 0x310/4
		icrl := 0x300/4
		// use sync to guarantee order
		atomic.StoreUint32(&lap[icrh], hi)
		atomic.StoreUint32(&lap[icrl], low)
		ipisent := uint32(1 << 12)
		for atomic.LoadUint32(&lap[icrl]) & ipisent != 0 {
		}
	}

	tlbshootvec := 70
	// broadcast shootdown
	low := ipilow(3, 0, tlbshootvec)
	icrw(0, low)

	// wait for other cpus to finish
	runtime.Tlbwait(mygen)
}

type bprof_t struct {
	data	[]byte
	c	int
}

func (b *bprof_t) init() {
	b.data = make([]byte, 0, 4096)
}

func (b *bprof_t) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *bprof_t) len() int {
	return len(b.data)
}

// dumps profile to serial console/vga for xxd -r
func (b *bprof_t) dump() {
	l := len(b.data)
	for i := 0; i < l; i += 16 {
		cur := b.data[i:]
		if len(cur) > 16 {
			cur = cur[:16]
		}
		fmt.Printf("%07x: ", i)
		prc := 0
		for _, b := range cur {
			fmt.Printf("%02x", b)
			prc++
			if prc % 2 == 0 {
				fmt.Printf(" ")
			}
		}
		fmt.Printf("\n")
	}
}

var prof = bprof_t{}

func cpuidfamily() (uint, uint) {
	ax, _, _, _ := runtime.Cpuid(1, 0)
	model :=  (ax >> 4) & 0xf
	family := (ax >> 8) & 0xf
	emodel := (ax >> 16) & 0xf
	efamily := (ax >> 20) & 0xff

	dispmodel := emodel << 4 + model
	dispfamily := efamily + family
	return uint(dispmodel), uint(dispfamily)
}

func cpuchk() {
	_, _, _, dx := runtime.Cpuid(0x80000001, 0)
	arch64 := uint32(1 << 29)
	if dx & arch64 == 0 {
		panic("not intel 64 arch?")
	}

	rmodel, rfamily := cpuidfamily()
	fmt.Printf("CPUID: family: %x, model: %x\n", rfamily, rmodel)

	ax, _, _, dx := runtime.Cpuid(1, 0)
	stepping := ax & 0xf
	oldp := rfamily == 6 && rmodel < 3 && stepping < 3
	sep := uint32(1 << 11)
	if dx & sep == 0 || oldp {
		panic("sysenter not supported")
	}

	_, _, _, dx = runtime.Cpuid(0x80000007, 0)
	invartsc := uint32(1 << 8)
	if dx & invartsc == 0 {
		// no qemu CPUs support invariant tsc, but my hardware does...
		//panic("invariant tsc not supported")
		fmt.Printf("invariant TSC not supported\n")
	}
}

func perfsetup() {
	ax, bx, _, _ := runtime.Cpuid(0xa, 0)
	perfv := ax & 0xff
	npmc := (ax >> 8) & 0xff
	pmcbits := (ax >> 16) & 0xff
	pmebits := (ax >> 24) & 0xff
	cyccnt := bx & 1 == 0
	_, _, cx, _ := runtime.Cpuid(0x1, 0)
	pdc := cx & (1 << 15) != 0
	if pdc && perfv >= 2 && perfv <= 3 && npmc >= 1 && pmebits >= 1 &&
	    cyccnt && pmcbits >= 32 {
		fmt.Printf("Hardware Performance monitoring enabled: " +
		    "%v counters\n", npmc)
		profhw = &intelprof_t{}
		profhw.prof_init(uint(npmc))
	} else {
		fmt.Printf("No hardware performance monitoring\n")
		profhw = &nilprof_t{}
	}
}

// performance monitoring event id
type pmevid_t uint

const(
	// if you modify the order of these flags, you must update them in libc
	// too.
	// architectural
	EV_UNHALTED_CORE_CYCLES		pmevid_t = 1 << iota
	EV_LLC_MISSES			pmevid_t = 1 << iota
	EV_LLC_REFS			pmevid_t = 1 << iota
	EV_BRANCH_INSTR_RETIRED		pmevid_t = 1 << iota
	EV_BRANCH_MISS_RETIRED		pmevid_t = 1 << iota
	EV_INSTR_RETIRED		pmevid_t = 1 << iota
	// non-architectural
	// "all TLB misses that cause a page walk"
	EV_DTLB_LOAD_MISS_ANY		pmevid_t = 1 << iota
	// "number of completed walks due to miss in sTLB"
	EV_DTLB_LOAD_MISS_STLB		pmevid_t = 1 << iota
	// "retired stores that missed in the dTLB"
	EV_STORE_DTLB_MISS		pmevid_t = 1 << iota
	EV_L2_LD_HITS			pmevid_t = 1 << iota
	// "Counts the number of misses in all levels of the ITLB which causes
	// a page walk."
	EV_ITLB_LOAD_MISS_ANY		pmevid_t = 1 << iota
)

type pmflag_t uint

const(
	EVF_OS				pmflag_t = 1 << iota
	EVF_USR				pmflag_t = 1 << iota
)

type pmev_t struct {
	evid	pmevid_t
	pflags	pmflag_t
}

var pmevid_names = map[pmevid_t]string{
	EV_UNHALTED_CORE_CYCLES: "Unhalted core cycles",
	EV_LLC_MISSES: "LLC misses",
	EV_LLC_REFS: "LLC references",
	EV_BRANCH_INSTR_RETIRED: "Branch instructions retired",
	EV_BRANCH_MISS_RETIRED: "Branch misses retired",
	EV_INSTR_RETIRED: "Instructions retired",
	EV_DTLB_LOAD_MISS_ANY: "dTLB load misses",
	EV_ITLB_LOAD_MISS_ANY: "iTLB load misses",
	EV_DTLB_LOAD_MISS_STLB: "sTLB misses",
	EV_STORE_DTLB_MISS: "Store dTLB misses",
	//EV_WTF1: "dummy 1",
	//EV_WTF2: "dummy 2",
	EV_L2_LD_HITS: "L2 load hits",
}

// a device driver for hardware profiling
type profhw_i interface {
	prof_init(uint)
	startpmc([]pmev_t) ([]int, bool)
	stoppmc([]int) []uint
	startnmi(pmevid_t, pmflag_t, uint, uint) bool
	stopnmi() []uintptr
}

var profhw profhw_i

type nilprof_t struct {
}

func (n *nilprof_t) prof_init(uint) {
}

func (n *nilprof_t) startpmc([]pmev_t) ([]int, bool) {
	return nil, false
}

func (n *nilprof_t) stoppmc([]int) []uint {
	return nil
}

func (n *nilprof_t) startnmi(pmevid_t, pmflag_t, uint, uint) bool {
	return false
}

func (n *nilprof_t) stopnmi() []uintptr {
	return nil
}

type intelprof_t struct {
	l		sync.Mutex
	pmcs		[]intelpmc_t
	events		map[pmevid_t]pmevent_t
}

type intelpmc_t struct {
	alloced		bool
	eventid		pmevid_t
}

type pmevent_t struct {
	event	int
	umask	int
}

func (ip *intelprof_t) _disableall() {
	ip._perfmaskipi()
}

func (ip *intelprof_t) _enableall() {
	ip._perfmaskipi()
}

func (ip *intelprof_t) _perfmaskipi() {
	lapaddr := 0xfee00000
	lap := (*[PGSIZE/4]uint32)(unsafe.Pointer(uintptr(lapaddr)))

	allandself := 2
	trap_perfmask := 72
	level := 1 << 14
	low := uint32(allandself << 18 | level | trap_perfmask)
	icrl := 0x300/4
	atomic.StoreUint32(&lap[icrl], low)
	ipisent := uint32(1 << 12)
	for atomic.LoadUint32(&lap[icrl]) & ipisent != 0 {
	}
}

func (ip *intelprof_t) _ev2msr(eid pmevid_t, pf pmflag_t) int {
	ev, ok := ip.events[eid]
	if !ok {
		panic("no such event")
	}
	usr := 1 << 16
	os  := 1 << 17
	en  := 1 << 22
	event := ev.event
	umask := ev.umask << 8
	v := umask | event | en
	if pf & EVF_OS != 0 {
		v |= os
	}
	if pf & EVF_USR != 0 {
		v |= usr
	}
	if pf == 0 {
		v |= os | usr
	}
	return v
}

// XXX counting PMCs only works with one CPU; move counter start/stop to perf
// IPI.
func (ip *intelprof_t) _pmc_start(cid int, eid pmevid_t, pf pmflag_t) {
	if cid < 0 || cid >= len(ip.pmcs) {
		panic("wtf")
	}
	wrmsr := func(a, b int) {
		runtime.Wrmsr(a, b)
	}
	ia32_pmc0 := 0xc1
	ia32_perfevtsel0 := 0x186
	pmc := ia32_pmc0 + cid
	evtsel := ia32_perfevtsel0 + cid
	// disable perf counter before clearing
	wrmsr(evtsel, 0)
	wrmsr(pmc, 0)

	v := ip._ev2msr(eid, pf)
	wrmsr(evtsel, v)
}

func (ip *intelprof_t) _pmc_stop(cid int) uint {
	if cid < 0 || cid >= len(ip.pmcs) {
		panic("wtf")
	}
	ia32_pmc0 := 0xc1
	ia32_perfevtsel0 := 0x186
	pmc := ia32_pmc0 + cid
	evtsel := ia32_perfevtsel0 + cid
	ret := runtime.Rdmsr(pmc)
	runtime.Wrmsr(evtsel, 0)
	return uint(ret)
}

func (ip *intelprof_t) prof_init(npmc uint) {
	ip.pmcs = make([]intelpmc_t, npmc)
	// architectural events
	ip.events = map[pmevid_t]pmevent_t{
	    EV_UNHALTED_CORE_CYCLES:
		{0x3c, 0},
	    EV_LLC_MISSES:
		{0x2e, 0x41},
	    EV_LLC_REFS:
		{0x2e, 0x4f},
	    EV_BRANCH_INSTR_RETIRED:
		{0xc4, 0x0},
	    EV_BRANCH_MISS_RETIRED:
		{0xc5, 0x0},
	    EV_INSTR_RETIRED:
		{0xc0, 0x0},
	}

	_xeon5000 := map[pmevid_t]pmevent_t{
	    EV_DTLB_LOAD_MISS_ANY:
		{0x08, 0x1},
	    EV_DTLB_LOAD_MISS_STLB:
		{0x08, 0x2},
	    EV_STORE_DTLB_MISS:
		{0x0c, 0x1},
	    EV_ITLB_LOAD_MISS_ANY:
		{0x85, 0x1},
	    //EV_WTF1:
	    //    {0x49, 0x1},
	    //EV_WTF2:
	    //    {0x14, 0x2},
	    EV_L2_LD_HITS:
		{0x24, 0x1},
	}

	dispmodel, dispfamily := cpuidfamily()

	if dispfamily == 0x6 && dispmodel == 0x1e {
		for k, v := range _xeon5000 {
			ip.events[k] = v
		}
	}
}

// starts a performance counter for each event in evs. if all the counters
// cannot be allocated, no performance counter is started.
func (ip *intelprof_t) startpmc(evs []pmev_t) ([]int, bool) {
	ip.l.Lock()
	defer ip.l.Unlock()

	// are the event ids supported?
	for _, ev := range evs {
		if _, ok := ip.events[ev.evid]; !ok {
			return nil, false
		}
	}
	// make sure we have enough counters
	cnt := 0
	for i := range ip.pmcs {
		if !ip.pmcs[i].alloced {
			cnt++
		}
	}
	if cnt < len(evs) {
		return nil, false
	}

	ret := make([]int, len(evs))
	ri := 0
	// find available counter
	outer:
	for _, ev := range evs {
		eid := ev.evid
		for i := range ip.pmcs {
			if !ip.pmcs[i].alloced {
				ip.pmcs[i].alloced = true
				ip.pmcs[i].eventid = eid
				ip._pmc_start(i, eid, ev.pflags)
				ret[ri] = i
				ri++
				continue outer
			}
		}
	}
	return ret, true
}

func (ip *intelprof_t) stoppmc(idxs []int) []uint {
	ip.l.Lock()
	defer ip.l.Unlock()

	ret := make([]uint, len(idxs))
	ri := 0
	for _, idx := range idxs {
		if !ip.pmcs[idx].alloced {
			ret[ri] = 0
			ri++
			continue
		}
		ip.pmcs[idx].alloced = false
		c := ip._pmc_stop(idx)
		ret[ri] = c
		ri++
	}
	return ret
}

func (ip *intelprof_t) startnmi(evid pmevid_t, pf pmflag_t, min,
    max uint) bool {
	ip.l.Lock()
	defer ip.l.Unlock()
	if ip.pmcs[0].alloced {
		return false
	}
	if _, ok := ip.events[evid]; !ok {
		return false
	}
	// NMI profiling currently only uses pmc0 (but could use any other
	// counter)
	ip.pmcs[0].alloced = true

	v := ip._ev2msr(evid, pf)
	// enable LVT interrupt on PMC overflow
	inte := 1 << 20
	v |= inte

	mask := false
	runtime.SetNMI(mask, v, min, max)
	ip._enableall()
	return true
}

func (ip *intelprof_t) stopnmi() []uintptr {
	ip.l.Lock()
	defer ip.l.Unlock()

	mask := true
	runtime.SetNMI(mask, 0, 0, 0)
	ip._disableall()
	buf, full := runtime.TakeNMIBuf()
	if full {
		fmt.Printf("*** NMI buffer is full!\n")
	}

	ip.pmcs[0].alloced = false

	return buf
}

func mkbm() {
	ch := make(chan bool)
	for it := 0; it < 20; it++ {
		st := runtime.Nanotime()
		n := st
		ops := 0
		ns := 3000000000
		for n - st < ns {
			go func() {
				ch <- true
			}()
			<- ch
			n = runtime.Nanotime()
			ops++
		}
		fmt.Printf("%20v ns/spawn+send (%v ops)\n", ns/ops, ops)
	}
}

func sendbm() {
	ch := make(chan bool)
	go func() {
		for {
			<- ch
			ch <- true
		}
	}()
	times := 3
	for it := 0; it < times; it++ {
		st := runtime.Nanotime()
		n := st
		ops := 0
		ns := 3000000000
		for n - st < ns {
			for i := 0; i < 1000; i++ {
				ch <- false
				<- ch
				ops += 2
			}
			n = runtime.Nanotime()
		}
		fmt.Printf("%20v ns/sends (%v ops)\n", ns/ops, ops)
	}
}

func insertbm() {
	fmt.Printf("making keys, growing m...")
	_buf := make([]byte, 100)
	randstr := func() string {
		n := 10
		for i := 0; i < n; i++ {
			_buf[i] = byte('0' + rand.Intn('Z' - '0'))
		}
		return string(_buf[:n])
	}

	m := make(map[string]bool)
	keyn := 1000000
	keys := make([]string, keyn)
	for i := range keys {
		if (i + 1) % (keyn/10) == 0 {
			fmt.Printf("|")
		}
		keys[i] = randstr()
		m[keys[i]] = true
	}
	for i := range m {
		delete(m, i)
	}
	fmt.Printf("done. made %v keys.\n", len(keys))

	times := 3
	for i := 0; i < times; i++ {
		st := runtime.Nanotime()
		for _, k := range keys {
			m[k] = true
		}
		elap := runtime.Nanotime() - st
		fmt.Printf("%20v ns/insert (no lock)\n", elap/len(keys))
		for i := range m {
			delete(m, i)
		}
	}

	l := sync.Mutex{}
	for i := 0; i < times; i++ {
		st := runtime.Nanotime()
		for _, k := range keys {
			l.Lock()
			m[k] = true
			l.Unlock()
		}
		elap := runtime.Nanotime() - st
		fmt.Printf("%20v ns/insert\n", elap/len(keys))
		for i := range m {
			delete(m, i)
		}
	}
}

// can account for up to 16TB of mem
type physpg_t struct {
	refcnt		int32
	// index into pgs of next page on free list
	nexti		uint32
}

var physmem struct {
	pgs		[]physpg_t
	startn		uint32
	// index into pgs of first free pg
	freei		uint32
	sync.Mutex
}

func _refaddr(p_pg uintptr) (*int32, uint32) {
	idx := _pg2pgn(p_pg) - physmem.startn
	return &physmem.pgs[idx].refcnt, idx
}

func refup(p_pg uintptr) {
	ref, _ := _refaddr(p_pg)
	c := atomic.AddInt32(ref, 1)
	// XXXPANIC
	if c <= 0 {
		panic("wut")
	}
}

// returns true if p_pg should be added to the free list and the index of the
// page in the pgs array
func _refdown(p_pg uintptr) (bool, uint32) {
	ref, idx := _refaddr(p_pg)
	c := atomic.AddInt32(ref, -1)
	// XXXPANIC
	if c < 0 {
		panic("wut")
	}
	return c == 0, idx
}

func _reffree(idx uint32) {
	physmem.Lock()
	onext := physmem.freei
	physmem.pgs[idx].nexti = onext
	physmem.freei = idx
	physmem.Unlock()
}

func refdown(p_pg uintptr) {
	// add to freelist?
	if add, idx :=_refdown(p_pg); add {
		_reffree(idx)
	}
}

func _refpg_new() (*[512]int, int) {
	if !_dmapinit {
		panic("dmap not initted")
	}

	physmem.Lock()
	firstfree := physmem.freei
	newhead := physmem.pgs[firstfree].nexti
	physmem.freei = newhead
	physmem.Unlock()

	if firstfree == ^uint32(0) {
		panic("refpgs oom")
	}

	pi := int(firstfree)
	p_pg := uintptr(firstfree + physmem.startn) << PGSHIFT

	if physmem.pgs[pi].refcnt < 0 {
		panic("how?")
	}
	dur := int(p_pg)
	pg := dmap(dur)
	return pg, dur
}

// refcnt of returned page is not incremented (it is usually incremented via
// proc_t.page_insert). requires direct mapping.
func refpg_new() (*[512]int, int) {
	pg, p_pg := _refpg_new()
	*pg = *zeropg
	return pg, p_pg
}

func _pg2pgn(p_pg uintptr) uint32 {
	return uint32(p_pg >> PGSHIFT)
}

func phys_init() {
	// reserve 128MB of pages
	//respgs := 1 << 15
	respgs := 1 << 16
	// 7.5 GB
	//respgs := 1835008
	//respgs := 1 << 18 + (1 <<17)
	physmem.pgs = make([]physpg_t, respgs)
	for i := range physmem.pgs {
		physmem.pgs[i].refcnt = -10
	}
	first := runtime.Get_phys()
	fpgn := _pg2pgn(first)
	physmem.startn = fpgn
	physmem.freei = 0
	physmem.pgs[0].refcnt = 0
	physmem.pgs[0].nexti = ^uint32(0)
	for i := 0; i < respgs - 1; i++ {
		p_pg := runtime.Get_phys()
		pgn := _pg2pgn(p_pg)
		idx := pgn - physmem.startn
		// Get_phys() may skip regions.
		if int(idx) >= len(physmem.pgs) {
			if respgs - i > int(float64(respgs)*0.01) {
				panic("got many bad pages")
			}
			break
		}
		physmem.pgs[idx].refcnt = 0
		physmem.pgs[idx].nexti = physmem.freei
		physmem.freei = idx
	}
	fmt.Printf("Reserved %v pages (%vMB)\n", respgs, respgs >> 8)
}

func pgcount() int {
	s := 0
	for i := physmem.freei; i != ^uint32(0); i = physmem.pgs[i].nexti {
		s++
	}
	return s
}

func main() {
	// magic loop
	//if rand.Int() != 0 {
	//	for {
	//	}
	//}
	phys_init()

	go func() {
		<- time.After(10*time.Second)
		fmt.Printf("[It is now safe to benchmark...]\n")
	}()

	fmt.Printf("              BiscuitOS\n");
	fmt.Printf("          go version: %v\n", runtime.Version())
	pmem := runtime.Totalphysmem()
	fmt.Printf("  %v MB of physical memory\n", pmem >> 20)

	cpuchk()

	dmap_init()
	perfsetup()
	// control CPUs
	aplim := 7

	//pci_dump()
	ncpu := attach_devs()

	if disk == nil || INT_DISK < 0 {
		panic("no disk")
	}

	// XXX pass irqs from attach_devs to trapstub, not global state.
	// must come before init funcs below
	runtime.Install_traphandler(trapstub)

	handlers := map[int]func(*trapstore_t) {
	     INT_DISK: trap_disk,
	     INT_KBD: trap_cons,
	     INT_COM1: trap_cons,
	     }
	go trap(handlers)

	cpus_start(ncpu, aplim)
	//runtime.SCenable = false

	kbd_init()

	rf := fs_init()
	use_memfs()

	exec := func(cmd string, args []string) {
		fmt.Printf("start [%v %v]\n", cmd, args)
		nargs := []string{cmd}
		nargs = append(nargs, args...)
		defaultfds := []*fd_t{&fd_stdin, &fd_stdout, &fd_stderr}
		p := proc_new(cmd, rf, defaultfds)
		var tf [TFSIZE]int
		ret := sys_execv1(p, &tf, cmd, nargs)
		if ret != 0 {
			panic(fmt.Sprintf("exec failed %v", ret))
		}
		p.sched_add(&tf, p.tid0)
	}

	//exec("bin/lsh", nil)
	exec("bin/init", nil)
	//exec("bin/rs", []string{"/redis.conf"})

	//go func() {
	//	d := time.Second
	//	for {
	//		<- time.After(d)
	//		ms := &runtime.MemStats{}
	//		runtime.ReadMemStats(ms)
	//		fmt.Printf("%v MiB\n", ms.Alloc/ (1 << 20))
	//	}
	//}()

	var dur chan bool
	<- dur
}

func findbm() {
	dmap_init()
	//n := incn()
	//var aplim int
	//if n == 0 {
	//	aplim = 1
	//} else {
	//	aplim = 7
	//}
	aplim := 7
	cpus_start(aplim, aplim)

	ch := make(chan bool)
	times := uint64(0)
	sum := uint64(0)
	for {
		st0 := runtime.Rdtsc()
		go func(st uint64) {
			tot := runtime.Rdtsc() - st
			sum += tot
			times++
			if times % 1000000 == 0 {
				fmt.Printf("%9v cycles to find (avg)\n",
				    sum/times)
				sum = 0
				times = 0
			}
			ch <- true
		}(st0)
		//<- ch
		loopy: for {
			select {
			case <- ch:
				break loopy
			default:
			}
		}
	}
}

func nvcount() int {
	var data [512]uint8
	req := idereq_new(0, false, &data)
	ide_request <- req
	<- req.ack
	ret := req.buf.data[505]
	req.buf.data[505] = ret + 1
	req.write = true
	ide_request <- req
	<- req.ack
	return int(ret)
}
