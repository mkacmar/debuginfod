package debuginfod

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"fmt"
)

// makeMinimalELF builds a minimal valid little-endian ELF64 file containing a single named section.
// Layout: [ELF header][section data][.shstrtab][section header table].
func makeMinimalELF(sectionName string, data []byte) []byte {
	// Section name table: leading NUL, then each name NUL-terminated.
	names := []byte{0}
	sectionNameOff := uint32(len(names))
	names = append(append(names, sectionName...), 0)
	shstrtabNameOff := uint32(len(names))
	names = append(append(names, ".shstrtab"...), 0)

	const headerSize = 64
	const sectionHeaderSize = 64
	dataOff := uint64(headerSize)
	namesOff := dataOff + uint64(len(data))
	sectionHeadersOff := namesOff + uint64(len(names))

	header := elf.Header64{
		Ident: [16]byte{
			0x7f, 'E', 'L', 'F',
			byte(elf.ELFCLASS64),
			byte(elf.ELFDATA2LSB),
			byte(elf.EV_CURRENT),
		},
		Type:      uint16(elf.ET_EXEC),
		Machine:   uint16(elf.EM_X86_64),
		Version:   uint32(elf.EV_CURRENT),
		Shoff:     sectionHeadersOff,
		Ehsize:    headerSize,
		Shentsize: sectionHeaderSize,
		Shnum:     3, // NULL + named section + .shstrtab
		Shstrndx:  2, // index of .shstrtab in the section header table
	}

	sections := []elf.Section64{
		{}, // SHN_UNDEF
		{
			Name:      sectionNameOff,
			Type:      uint32(elf.SHT_PROGBITS),
			Off:       dataOff,
			Size:      uint64(len(data)),
			Addralign: 1,
		},
		{
			Name:      shstrtabNameOff,
			Type:      uint32(elf.SHT_STRTAB),
			Off:       namesOff,
			Size:      uint64(len(names)),
			Addralign: 1,
		},
	}

	var buf bytes.Buffer
	mustWrite(&buf, &header)
	buf.Write(data)
	buf.Write(names)
	for i := range sections {
		mustWrite(&buf, &sections[i])
	}
	return buf.Bytes()
}

func mustWrite(buf *bytes.Buffer, v any) {
	if err := binary.Write(buf, binary.LittleEndian, v); err != nil {
		panic(fmt.Sprintf("test setup: binary.Write: %v", err))
	}
}
