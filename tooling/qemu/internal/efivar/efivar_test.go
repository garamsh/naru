package efivar_test

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/efivar"
)

var esp = efivar.ESP{
	Number: 1, Start: 2048, Sectors: 204800,
	GUID: [16]byte{0xC1, 0xE8, 0xD2, 0xA4, 0x37, 0x5B, 0x90, 0x4F, 0x8E, 0x16, 0x2C, 0x7B, 0x9D, 0x0A, 0x3E, 0x45},
}

func TestDevicePath_IsAHardDriveNodeAFileNodeAndTheEnd(t *testing.T) {
	p := esp.DevicePath(`\EFI\BOOT\TRIAL.EFI`)

	// Hard drive node: type 4, subtype 1, length 42.
	assert.Equal(t, []byte{4, 1, 42, 0}, p[:4])
	assert.Equal(t, uint32(1), binary.LittleEndian.Uint32(p[4:]), "partition number")
	assert.Equal(t, uint64(2048), binary.LittleEndian.Uint64(p[8:]), "start")
	assert.Equal(t, uint64(204800), binary.LittleEndian.Uint64(p[16:]), "size")
	assert.Equal(t, esp.GUID[:], p[24:40], "the GUID as the GPT stores it")
	assert.Equal(t, []byte{2, 2}, p[40:42], "GPT, GUID signature")
	// File path node: type 4, subtype 4, length 4 plus the null-terminated UCS-2 name.
	name := `\EFI\BOOT\TRIAL.EFI`
	assert.Equal(t, []byte{4, 4}, p[42:44])
	assert.Equal(t, uint16(4+2*(len(name)+1)), binary.LittleEndian.Uint16(p[44:]))
	assert.Equal(t, []byte{'\\', 0, 'E', 0}, p[46:50])
	// End of the device path.
	assert.Equal(t, []byte{0x7f, 0xff, 4, 0}, p[len(p)-4:])
	assert.Len(t, p, 42+4+2*(len(name)+1)+4)
}

func TestLoadOption_IsActiveCarriesItsPathLengthAndDescription(t *testing.T) {
	path := esp.DevicePath(`\EFI\BOOT\TRIAL.EFI`)

	o := efivar.LoadOption("naru", path)

	assert.Equal(t, uint32(1), binary.LittleEndian.Uint32(o), "LOAD_OPTION_ACTIVE")
	assert.Equal(t, uint16(len(path)), binary.LittleEndian.Uint16(o[4:]))
	assert.Equal(t, []byte{'n', 0, 'a', 0, 'r', 0, 'u', 0, 0, 0}, o[6:16], "the description, null-terminated")
	assert.Equal(t, path, o[16:])
}

func TestBoot_NamesTheEntryInFourUpperCaseHexDigits(t *testing.T) {
	assert.Equal(t, "Boot0100", efivar.Boot(0x100))
	assert.Equal(t, "Boot00AF", efivar.Boot(0xAF))
}

func TestWriteVariable_PutsTheAttributesFirstAndReadVariableDropsThem(t *testing.T) {
	dir := t.TempDir()

	require.NoError(t, efivar.SetBootNext(dir, 0x0100))

	raw, err := os.ReadFile(filepath.Join(dir, "BootNext-"+efivar.GlobalGUID))
	require.NoError(t, err)
	assert.Equal(t, []byte{7, 0, 0, 0, 0x00, 0x01}, raw, "NV|BS|RT, then the number little endian")
	n, err := efivar.Uint16(dir, "BootNext")
	require.NoError(t, err)
	assert.Equal(t, uint16(0x100), n)
}

func TestWriteVariable_ReplacesAVariableThatExists(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, efivar.SetBootNext(dir, 1))

	require.NoError(t, efivar.SetBootNext(dir, 2))

	n, err := efivar.Uint16(dir, "BootNext")
	require.NoError(t, err)
	assert.Equal(t, uint16(2), n)
}

func TestUint16_ReportsAVariableThatIsNotThere(t *testing.T) {
	_, err := efivar.Uint16(t.TempDir(), "BootNext")

	assert.ErrorIs(t, err, os.ErrNotExist)
}
