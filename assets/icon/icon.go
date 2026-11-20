package icon

import (
	"bytes"
	"debug/pe"
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
)

//go:embed kimi-work-plus.ico
var WindowsICO []byte

const (
	// AppIconResourceID matches Wails v3 Windows window/tray icon lookup (ID 3)
	// and serves as index 0 for Explorer shortcuts and Add/Remove Programs.
	AppIconResourceID uint16 = 3

	resourceTypeIcon      uint32 = 3
	resourceTypeGroupIcon uint32 = 14
	resourceLangENUS      uint32 = 0x0409

	coffRelocAMD64Addr32NB uint16 = 0x0003
	coffSymClassStatic     uint8  = 3
)

type ICOEntry struct {
	Width      int
	Height     int
	ColorCount byte
	Reserved   byte
	Planes     uint16
	BitCount   uint16
	Data       []byte
}

func ParseICO(raw []byte) ([]ICOEntry, error) {
	if len(raw) < 6 {
		return nil, errors.New("ico too short")
	}
	reserved := binary.LittleEndian.Uint16(raw[0:2])
	typ := binary.LittleEndian.Uint16(raw[2:4])
	count := int(binary.LittleEndian.Uint16(raw[4:6]))
	if reserved != 0 || typ != 1 || count <= 0 {
		return nil, fmt.Errorf("invalid ico header: reserved=%d type=%d count=%d", reserved, typ, count)
	}
	dirEnd := 6 + count*16
	if len(raw) < dirEnd {
		return nil, errors.New("ico directory truncated")
	}
	entries := make([]ICOEntry, 0, count)
	for i := 0; i < count; i++ {
		off := 6 + i*16
		w := int(raw[off])
		if w == 0 {
			w = 256
		}
		h := int(raw[off+1])
		if h == 0 {
			h = 256
		}
		size := binary.LittleEndian.Uint32(raw[off+8 : off+12])
		dataOff := binary.LittleEndian.Uint32(raw[off+12 : off+16])
		end := uint64(dataOff) + uint64(size)
		if size == 0 || uint64(dataOff) < uint64(dirEnd) || end > uint64(len(raw)) {
			return nil, fmt.Errorf("ico entry %d out of bounds", i)
		}
		entries = append(entries, ICOEntry{
			Width:      w,
			Height:     h,
			ColorCount: raw[off+2],
			Reserved:   raw[off+3],
			Planes:     binary.LittleEndian.Uint16(raw[off+4 : off+6]),
			BitCount:   binary.LittleEndian.Uint16(raw[off+6 : off+8]),
			Data:       bytes.Clone(raw[dataOff : dataOff+size]),
		})
	}
	return entries, nil
}

func BuildAMD64ResourceSyso(ico []byte, groupID uint16) ([]byte, error) {
	if groupID == 0 {
		return nil, errors.New("group icon resource id must be non-zero")
	}
	entries, err := ParseICO(ico)
	if err != nil {
		return nil, err
	}
	if len(entries) > 0x7ffe {
		return nil, fmt.Errorf("too many ico entries: %d", len(entries))
	}

	groupData := make([]byte, 6+len(entries)*14)
	binary.LittleEndian.PutUint16(groupData[0:2], 0)
	binary.LittleEndian.PutUint16(groupData[2:4], 1)
	binary.LittleEndian.PutUint16(groupData[4:6], uint16(len(entries)))
	for i, e := range entries {
		off := 6 + i*14
		if e.Width != 256 {
			groupData[off] = byte(e.Width)
		}
		if e.Height != 256 {
			groupData[off+1] = byte(e.Height)
		}
		groupData[off+2] = e.ColorCount
		groupData[off+3] = e.Reserved
		binary.LittleEndian.PutUint16(groupData[off+4:off+6], e.Planes)
		binary.LittleEndian.PutUint16(groupData[off+6:off+8], e.BitCount)
		binary.LittleEndian.PutUint32(groupData[off+8:off+12], uint32(len(e.Data)))
		binary.LittleEndian.PutUint16(groupData[off+12:off+14], uint16(i+1))
	}

	n := len(entries)
	leafCount := n + 1

	rootDirSize := 16 + 2*8
	iconDirOff := uint32(rootDirSize)
	iconDirSize := 16 + n*8
	groupDirOff := iconDirOff + uint32(iconDirSize)
	groupDirSize := 16 + 8
	langBaseOff := groupDirOff + uint32(groupDirSize)
	langDirSize := 16 + 8
	dataEntryBaseOff := langBaseOff + uint32(leafCount*langDirSize)
	rawBaseOff := dataEntryBaseOff + uint32(leafCount*16)

	blobs := make([][]byte, leafCount)
	for i, e := range entries {
		blobs[i] = e.Data
	}
	blobs[n] = groupData

	blobOffsets := make([]uint32, leafCount)
	cur := rawBaseOff
	for i, b := range blobs {
		cur = align4(cur)
		blobOffsets[i] = cur
		cur += uint32(len(b))
	}
	rsrcSize := align4(cur)
	rsrc := make([]byte, rsrcSize)

	writeDir := func(off uint32, idEntries []struct{ id, val uint32 }) {
		binary.LittleEndian.PutUint16(rsrc[off+14:off+16], uint16(len(idEntries)))
		for i, ent := range idEntries {
			pos := off + 16 + uint32(i*8)
			binary.LittleEndian.PutUint32(rsrc[pos:pos+4], ent.id)
			binary.LittleEndian.PutUint32(rsrc[pos+4:pos+8], ent.val)
		}
	}

	const subdirFlag uint32 = 0x80000000
	writeDir(0, []struct{ id, val uint32 }{
		{resourceTypeIcon, subdirFlag | iconDirOff},
		{resourceTypeGroupIcon, subdirFlag | groupDirOff},
	})

	iconDirEntries := make([]struct{ id, val uint32 }, n)
	for i := 0; i < n; i++ {
		iconDirEntries[i] = struct{ id, val uint32 }{
			id:  uint32(i + 1),
			val: subdirFlag | (langBaseOff + uint32(i*langDirSize)),
		}
	}
	writeDir(iconDirOff, iconDirEntries)
	writeDir(groupDirOff, []struct{ id, val uint32 }{
		{uint32(groupID), subdirFlag | (langBaseOff + uint32(n*langDirSize))},
	})

	type coffReloc struct {
		va  uint32
		sym uint32
		typ uint16
	}
	relocs := make([]coffReloc, leafCount)
	for i := 0; i < leafCount; i++ {
		langOff := langBaseOff + uint32(i*langDirSize)
		deOff := dataEntryBaseOff + uint32(i*16)
		writeDir(langOff, []struct{ id, val uint32 }{
			{resourceLangENUS, deOff},
		})
		binary.LittleEndian.PutUint32(rsrc[deOff:deOff+4], blobOffsets[i])
		binary.LittleEndian.PutUint32(rsrc[deOff+4:deOff+8], uint32(len(blobs[i])))
		relocs[i] = coffReloc{
			va:  deOff,
			sym: 0,
			typ: coffRelocAMD64Addr32NB,
		}
		copy(rsrc[blobOffsets[i]:], blobs[i])
	}

	const (
		fileHeaderSize = 20
		sectHeaderSize = 40
	)
	rawDataPtr := uint32(fileHeaderSize + sectHeaderSize)
	relocPtr := rawDataPtr + rsrcSize
	symPtr := relocPtr + uint32(len(relocs)*10)

	var out bytes.Buffer
	out.Grow(int(symPtr) + 18 + 4)

	// COFF file header
	_ = binary.Write(&out, binary.LittleEndian, struct {
		Machine              uint16
		NumberOfSections     uint16
		TimeDateStamp        uint32
		PointerToSymbolTable uint32
		NumberOfSymbols      uint32
		SizeOfOptionalHeader uint16
		Characteristics      uint16
	}{
		Machine:              pe.IMAGE_FILE_MACHINE_AMD64,
		NumberOfSections:     1,
		PointerToSymbolTable: symPtr,
		NumberOfSymbols:      1,
	})

	// Section header (.rsrc)
	var sectName [8]byte
	copy(sectName[:], ".rsrc")
	_ = binary.Write(&out, binary.LittleEndian, struct {
		Name                 [8]byte
		VirtualSize          uint32
		VirtualAddress       uint32
		SizeOfRawData        uint32
		PointerToRawData     uint32
		PointerToRelocations uint32
		PointerToLineNumbers uint32
		NumberOfRelocations  uint16
		NumberOfLineNumbers  uint16
		Characteristics      uint32
	}{
		Name:                 sectName,
		SizeOfRawData:        rsrcSize,
		PointerToRawData:     rawDataPtr,
		PointerToRelocations: relocPtr,
		NumberOfRelocations:  uint16(len(relocs)),
		Characteristics:      pe.IMAGE_SCN_CNT_INITIALIZED_DATA | pe.IMAGE_SCN_MEM_READ,
	})

	out.Write(rsrc)
	for _, rel := range relocs {
		_ = binary.Write(&out, binary.LittleEndian, rel.va)
		_ = binary.Write(&out, binary.LittleEndian, rel.sym)
		_ = binary.Write(&out, binary.LittleEndian, rel.typ)
	}

	// Single static section symbol for .rsrc
	_ = binary.Write(&out, binary.LittleEndian, struct {
		Name               [8]byte
		Value              uint32
		SectionNumber      int16
		Type               uint16
		StorageClass       uint8
		NumberOfAuxSymbols uint8
	}{
		Name:          sectName,
		SectionNumber: 1,
		StorageClass:  coffSymClassStatic,
	})
	// Empty COFF string table
	_ = binary.Write(&out, binary.LittleEndian, uint32(4))

	return out.Bytes(), nil
}

type PEIconSummary struct {
	IconIDs      []uint16
	GroupIconIDs []uint16
}

func InspectPEIcons(raw []byte) (PEIconSummary, error) {
	f, err := pe.NewFile(bytes.NewReader(raw))
	if err != nil {
		return PEIconSummary{}, err
	}
	defer f.Close()

	var rsrcSect *pe.Section
	for _, s := range f.Sections {
		if s.Name == ".rsrc" {
			rsrcSect = s
			break
		}
	}
	if rsrcSect == nil {
		return PEIconSummary{}, errors.New("PE missing .rsrc section")
	}
	if oh, ok := f.OptionalHeader.(*pe.OptionalHeader64); ok {
		dir := oh.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_RESOURCE]
		if dir.VirtualAddress == 0 || dir.Size == 0 {
			return PEIconSummary{}, errors.New("PE optional header missing resource directory entry")
		}
	}
	data, err := rsrcSect.Data()
	if err != nil {
		return PEIconSummary{}, err
	}
	return inspectResourceTree(data, rsrcSect.VirtualAddress)
}

func inspectResourceTree(rsrc []byte, sectVA uint32) (PEIconSummary, error) {
	type dirEntry struct {
		id  uint32
		off uint32
		sub bool
	}
	readDir := func(off uint32) ([]dirEntry, error) {
		if uint64(off)+16 > uint64(len(rsrc)) {
			return nil, fmt.Errorf("resource directory at 0x%x out of range", off)
		}
		named := int(binary.LittleEndian.Uint16(rsrc[off+12 : off+14]))
		idCnt := int(binary.LittleEndian.Uint16(rsrc[off+14 : off+16]))
		total := named + idCnt
		if uint64(off)+16+uint64(total*8) > uint64(len(rsrc)) {
			return nil, fmt.Errorf("resource directory entries at 0x%x truncated", off)
		}
		out := make([]dirEntry, 0, idCnt)
		for i := named; i < total; i++ {
			pos := off + 16 + uint32(i*8)
			id := binary.LittleEndian.Uint32(rsrc[pos : pos+4])
			val := binary.LittleEndian.Uint32(rsrc[pos+4 : pos+8])
			out = append(out, dirEntry{
				id:  id,
				off: val & 0x7fffffff,
				sub: val&0x80000000 != 0,
			})
		}
		return out, nil
	}

	validateLeaf := func(langDirOff uint32) error {
		langs, err := readDir(langDirOff)
		if err != nil || len(langs) == 0 {
			return fmt.Errorf("missing language entry at 0x%x", langDirOff)
		}
		for _, l := range langs {
			if l.sub {
				return fmt.Errorf("unexpected subdir at language level 0x%x", l.off)
			}
			if uint64(l.off)+16 > uint64(len(rsrc)) {
				return fmt.Errorf("resource data entry at 0x%x out of range", l.off)
			}
			rva := binary.LittleEndian.Uint32(rsrc[l.off : l.off+4])
			size := binary.LittleEndian.Uint32(rsrc[l.off+4 : l.off+8])
			if rva < sectVA {
				return fmt.Errorf("resource data RVA 0x%x below section VA 0x%x", rva, sectVA)
			}
			rel := rva - sectVA
			if size == 0 || uint64(rel)+uint64(size) > uint64(len(rsrc)) {
				return fmt.Errorf("resource data blob at 0x%x size %d out of range", rel, size)
			}
		}
		return nil
	}

	root, err := readDir(0)
	if err != nil {
		return PEIconSummary{}, err
	}
	var summary PEIconSummary
	for _, typ := range root {
		if !typ.sub {
			continue
		}
		if typ.id != resourceTypeIcon && typ.id != resourceTypeGroupIcon {
			continue
		}
		names, err := readDir(typ.off)
		if err != nil {
			return PEIconSummary{}, err
		}
		for _, n := range names {
			if !n.sub {
				continue
			}
			if err := validateLeaf(n.off); err != nil {
				return PEIconSummary{}, err
			}
			if typ.id == resourceTypeIcon {
				summary.IconIDs = append(summary.IconIDs, uint16(n.id))
			} else {
				summary.GroupIconIDs = append(summary.GroupIconIDs, uint16(n.id))
			}
		}
	}
	return summary, nil
}

func align4(v uint32) uint32 {
	return (v + 3) &^ 3
}
