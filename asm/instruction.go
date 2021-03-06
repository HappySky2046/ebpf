package asm

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"strings"

	"golang.org/x/xerrors"
)

// InstructionSize is the size of a BPF instruction in bytes
const InstructionSize = 8

// Instruction is a single eBPF instruction.
type Instruction struct {
	OpCode    OpCode
	Dst       Register
	Src       Register
	Offset    int16
	Constant  int64
	Reference string
	Symbol    string
}

// Sym creates a symbol.
func (ins Instruction) Sym(name string) Instruction {
	ins.Symbol = name
	return ins
}

// Unmarshal decodes a BPF instruction.
func (ins *Instruction) Unmarshal(r io.Reader, bo binary.ByteOrder) (uint64, error) {
	var bi bpfInstruction
	err := binary.Read(r, bo, &bi)
	if err != nil {
		return 0, err
	}

	ins.OpCode = bi.OpCode
	ins.Dst = bi.Registers.Dst()
	ins.Src = bi.Registers.Src()
	ins.Offset = bi.Offset
	ins.Constant = int64(bi.Constant)

	if !bi.OpCode.isDWordLoad() {
		return InstructionSize, nil
	}

	var bi2 bpfInstruction
	if err := binary.Read(r, bo, &bi2); err != nil {
		// No Wrap, to avoid io.EOF clash
		return 0, xerrors.New("64bit immediate is missing second half")
	}
	if bi2.OpCode != 0 || bi2.Offset != 0 || bi2.Registers != 0 {
		return 0, xerrors.New("64bit immediate has non-zero fields")
	}
	ins.Constant = int64(uint64(uint32(bi2.Constant))<<32 | uint64(uint32(bi.Constant)))

	return 2 * InstructionSize, nil
}

// Marshal encodes a BPF instruction.
func (ins Instruction) Marshal(w io.Writer, bo binary.ByteOrder) (uint64, error) {
	if ins.OpCode == InvalidOpCode {
		return 0, xerrors.New("invalid opcode")
	}

	isDWordLoad := ins.OpCode.isDWordLoad()

	cons := int32(ins.Constant)
	if isDWordLoad {
		// Encode least significant 32bit first for 64bit operations.
		cons = int32(uint32(ins.Constant))
	}

	bpfi := bpfInstruction{
		ins.OpCode,
		newBPFRegisters(ins.Dst, ins.Src),
		ins.Offset,
		cons,
	}

	if err := binary.Write(w, bo, &bpfi); err != nil {
		return 0, err
	}

	if !isDWordLoad {
		return InstructionSize, nil
	}

	bpfi = bpfInstruction{
		Constant: int32(ins.Constant >> 32),
	}

	if err := binary.Write(w, bo, &bpfi); err != nil {
		return 0, err
	}

	return 2 * InstructionSize, nil
}

// RewriteMapPtr changes an instruction to use a new map fd.
//
// Returns an error if the instruction doesn't load a map.
func (ins *Instruction) RewriteMapPtr(fd int) error {
	if !ins.OpCode.isDWordLoad() {
		return xerrors.Errorf("%s is not a 64 bit load", ins.OpCode)
	}

	if ins.Src != PseudoMapFD && ins.Src != PseudoMapValue {
		return xerrors.New("not a load from a map")
	}

	// Preserve the offset value for direct map loads.
	offset := uint64(ins.Constant) & (math.MaxUint32 << 32)
	rawFd := uint64(uint32(fd))
	ins.Constant = int64(offset | rawFd)
	return nil
}

func (ins *Instruction) mapPtr() uint32 {
	return uint32(uint64(ins.Constant) & math.MaxUint32)
}

// RewriteMapOffset changes the offset of a direct load from a map.
//
// Returns an error if the instruction is not a direct load.
func (ins *Instruction) RewriteMapOffset(offset uint32) error {
	if !ins.OpCode.isDWordLoad() {
		return xerrors.Errorf("%s is not a 64 bit load", ins.OpCode)
	}

	if ins.Src != PseudoMapValue {
		return xerrors.New("not a direct load from a map")
	}

	fd := uint64(ins.Constant) & math.MaxUint32
	ins.Constant = int64(uint64(offset)<<32 | fd)
	return nil
}

func (ins *Instruction) mapOffset() uint32 {
	return uint32(uint64(ins.Constant) >> 32)
}

func (ins *Instruction) isLoadFromMap() bool {
	return ins.OpCode == LoadImmOp(DWord) && (ins.Src == PseudoMapFD || ins.Src == PseudoMapValue)
}

// Format implements fmt.Formatter.
func (ins Instruction) Format(f fmt.State, c rune) {
	if c != 'v' {
		fmt.Fprintf(f, "{UNRECOGNIZED: %c}", c)
		return
	}

	op := ins.OpCode

	if op == InvalidOpCode {
		fmt.Fprint(f, "INVALID")
		return
	}

	// Omit trailing space for Exit
	if op.JumpOp() == Exit {
		fmt.Fprint(f, op)
		return
	}

	if ins.isLoadFromMap() {
		fd := int32(ins.mapPtr())
		switch ins.Src {
		case PseudoMapFD:
			fmt.Fprintf(f, "LoadMapPtr dst: %s fd: %d", ins.Dst, fd)

		case PseudoMapValue:
			fmt.Fprintf(f, "LoadMapValue dst: %s, fd: %d off: %d", ins.Dst, fd, ins.mapOffset())
		}

		goto ref
	}

	fmt.Fprintf(f, "%v ", op)
	switch cls := op.Class(); cls {
	case LdClass, LdXClass, StClass, StXClass:
		switch op.Mode() {
		case ImmMode:
			fmt.Fprintf(f, "dst: %s imm: %d", ins.Dst, ins.Constant)
		case AbsMode:
			fmt.Fprintf(f, "imm: %d", ins.Constant)
		case IndMode:
			fmt.Fprintf(f, "dst: %s src: %s imm: %d", ins.Dst, ins.Src, ins.Constant)
		case MemMode:
			fmt.Fprintf(f, "dst: %s src: %s off: %d imm: %d", ins.Dst, ins.Src, ins.Offset, ins.Constant)
		case XAddMode:
			fmt.Fprintf(f, "dst: %s src: %s", ins.Dst, ins.Src)
		}

	case ALU64Class, ALUClass:
		fmt.Fprintf(f, "dst: %s ", ins.Dst)
		if op.ALUOp() == Swap || op.Source() == ImmSource {
			fmt.Fprintf(f, "imm: %d", ins.Constant)
		} else {
			fmt.Fprintf(f, "src: %s", ins.Src)
		}

	case JumpClass:
		switch jop := op.JumpOp(); jop {
		case Call:
			if ins.Src == PseudoCall {
				// bpf-to-bpf call
				fmt.Fprint(f, ins.Constant)
			} else {
				fmt.Fprint(f, BuiltinFunc(ins.Constant))
			}

		default:
			fmt.Fprintf(f, "dst: %s off: %d ", ins.Dst, ins.Offset)
			if op.Source() == ImmSource {
				fmt.Fprintf(f, "imm: %d", ins.Constant)
			} else {
				fmt.Fprintf(f, "src: %s", ins.Src)
			}
		}
	}

ref:
	if ins.Reference != "" {
		fmt.Fprintf(f, " <%s>", ins.Reference)
	}
}

// Instructions is an eBPF program.
type Instructions []Instruction

func (insns Instructions) String() string {
	return fmt.Sprint(insns)
}

// RewriteMapPtr rewrites all loads of a specific map pointer to a new fd.
//
// Returns an error if the symbol isn't used, see IsUnreferencedSymbol.
func (insns Instructions) RewriteMapPtr(symbol string, fd int) error {
	if symbol == "" {
		return xerrors.New("empty symbol")
	}

	found := false
	for i := range insns {
		ins := &insns[i]
		if ins.Reference != symbol {
			continue
		}

		if err := ins.RewriteMapPtr(fd); err != nil {
			return err
		}

		found = true
	}

	if !found {
		return &unreferencedSymbolError{symbol}
	}

	return nil
}

// SymbolOffsets returns the set of symbols and their offset in
// the instructions.
func (insns Instructions) SymbolOffsets() (map[string]int, error) {
	offsets := make(map[string]int)

	for i, ins := range insns {
		if ins.Symbol == "" {
			continue
		}

		if _, ok := offsets[ins.Symbol]; ok {
			return nil, xerrors.Errorf("duplicate symbol %s", ins.Symbol)
		}

		offsets[ins.Symbol] = i
	}

	return offsets, nil
}

// ReferenceOffsets returns the set of references and their offset in
// the instructions.
func (insns Instructions) ReferenceOffsets() map[string][]int {
	offsets := make(map[string][]int)

	for i, ins := range insns {
		if ins.Reference == "" {
			continue
		}

		offsets[ins.Reference] = append(offsets[ins.Reference], i)
	}

	return offsets
}

func (insns Instructions) marshalledOffsets() (map[string]int, error) {
	symbols := make(map[string]int)

	marshalledPos := 0
	for _, ins := range insns {
		currentPos := marshalledPos
		marshalledPos += ins.OpCode.marshalledInstructions()

		if ins.Symbol == "" {
			continue
		}

		if _, ok := symbols[ins.Symbol]; ok {
			return nil, xerrors.Errorf("duplicate symbol %s", ins.Symbol)
		}

		symbols[ins.Symbol] = currentPos
	}

	return symbols, nil
}

// Format implements fmt.Formatter.
//
// You can control indentation of symbols by
// specifying a width. Setting a precision controls the indentation of
// instructions.
// The default character is a tab, which can be overriden by specifying
// the ' ' space flag.
func (insns Instructions) Format(f fmt.State, c rune) {
	if c != 's' && c != 'v' {
		fmt.Fprintf(f, "{UNKNOWN FORMAT '%c'}", c)
		return
	}

	// Precision is better in this case, because it allows
	// specifying 0 padding easily.
	padding, ok := f.Precision()
	if !ok {
		padding = 1
	}

	indent := strings.Repeat("\t", padding)
	if f.Flag(' ') {
		indent = strings.Repeat(" ", padding)
	}

	symPadding, ok := f.Width()
	if !ok {
		symPadding = padding - 1
	}
	if symPadding < 0 {
		symPadding = 0
	}

	symIndent := strings.Repeat("\t", symPadding)
	if f.Flag(' ') {
		symIndent = strings.Repeat(" ", symPadding)
	}

	// Figure out how many digits we need to represent the highest
	// offset.
	highestOffset := 0
	for _, ins := range insns {
		highestOffset += ins.OpCode.marshalledInstructions()
	}
	offsetWidth := int(math.Ceil(math.Log10(float64(highestOffset))))

	offset := 0
	for _, ins := range insns {
		if ins.Symbol != "" {
			fmt.Fprintf(f, "%s%s:\n", symIndent, ins.Symbol)
		}
		fmt.Fprintf(f, "%s%*d: %v\n", indent, offsetWidth, offset, ins)
		offset += ins.OpCode.marshalledInstructions()
	}

	return
}

// Marshal encodes a BPF program into the kernel format.
func (insns Instructions) Marshal(w io.Writer, bo binary.ByteOrder) error {
	absoluteOffsets, err := insns.marshalledOffsets()
	if err != nil {
		return err
	}

	num := 0
	for i, ins := range insns {
		switch {
		case ins.OpCode.JumpOp() == Call && ins.Constant == -1:
			// Rewrite bpf to bpf call
			offset, ok := absoluteOffsets[ins.Reference]
			if !ok {
				return xerrors.Errorf("instruction %d: reference to missing symbol %s", i, ins.Reference)
			}

			ins.Constant = int64(offset - num - 1)

		case ins.OpCode.Class() == JumpClass && ins.Offset == -1:
			// Rewrite jump to label
			offset, ok := absoluteOffsets[ins.Reference]
			if !ok {
				return xerrors.Errorf("instruction %d: reference to missing symbol %s", i, ins.Reference)
			}

			ins.Offset = int16(offset - num - 1)
		}

		n, err := ins.Marshal(w, bo)
		if err != nil {
			return xerrors.Errorf("instruction %d: %w", i, err)
		}

		num += int(n / InstructionSize)
	}
	return nil
}

type bpfInstruction struct {
	OpCode    OpCode
	Registers bpfRegisters
	Offset    int16
	Constant  int32
}

type bpfRegisters uint8

func newBPFRegisters(dst, src Register) bpfRegisters {
	return bpfRegisters((src << 4) | (dst & 0xF))
}

func (r bpfRegisters) Dst() Register {
	return Register(r & 0xF)
}

func (r bpfRegisters) Src() Register {
	return Register(r >> 4)
}

type unreferencedSymbolError struct {
	symbol string
}

func (use *unreferencedSymbolError) Error() string {
	return fmt.Sprintf("unreferenced symbol %s", use.symbol)
}

// IsUnreferencedSymbol returns true if err was caused by
// an unreferenced symbol.
func IsUnreferencedSymbol(err error) bool {
	_, ok := err.(*unreferencedSymbolError)
	return ok
}
