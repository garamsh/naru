// Command naru-bootnext is the service the QEMU test of A/B boot configures (ADR 0020). On a boot
// where the ESP has no trial slot it copies the running kernel there, creates the firmware boot
// entry for it, sets BootNext and says so. On every boot it says which entry the firmware booted
// (BootCurrent), whether BootNext is still set, and whether the trial slot exists, and then waits.
// The test reads those lines from the console of each boot.
package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/garamsh/naru/tooling/qemu/internal/efivar"
)

// The trial entry's number; no BootOrder lists it, so only BootNext reaches it.
const trialEntry = 0x0100

func main() {
	disk := flag.String("disk", "/dev/sda", "the boot disk; its first partition is the ESP")
	flag.Parse()
	if err := run(*disk); err != nil {
		fmt.Println("naru56-efi: failed:", err)
		os.Exit(1)
	}
	time.Sleep(time.Hour)
}

func run(disk string) error {
	if err := os.MkdirAll(efivar.Dir, 0o755); err != nil {
		return err
	}
	if err := syscall.Mount("efivarfs", efivar.Dir, "efivarfs", 0, ""); err != nil && !errors.Is(err, syscall.EBUSY) {
		return fmt.Errorf("mount efivarfs: %w", err)
	}
	esp := "/run/naru56-esp"
	if err := os.MkdirAll(esp, 0o755); err != nil {
		return err
	}
	if err := syscall.Mount(disk+"1", esp, "vfat", 0, ""); err != nil {
		return fmt.Errorf("mount the ESP: %w", err)
	}
	defer func() { _ = syscall.Unmount(esp, 0) }()

	trial := filepath.Join(esp, "EFI/BOOT/TRIAL.EFI")
	if _, err := os.Stat(trial); errors.Is(err, os.ErrNotExist) {
		if err := arm(disk, esp, trial); err != nil {
			return err
		}
		fmt.Printf("naru56-efi: armed %s and BootNext=%04X\n", efivar.Boot(trialEntry), trialEntry)
	} else if err != nil {
		return err
	}
	_, err := os.Stat(trial)
	cur, curErr := efivar.Uint16(efivar.Dir, "BootCurrent")
	next, nextErr := efivar.Uint16(efivar.Dir, "BootNext")
	fmt.Printf("naru56-efi: BootCurrent=%s BootNext=%s trial=%t\n", show(cur, curErr), show(next, nextErr), err == nil)
	return nil
}

func show(n uint16, err error) string {
	if err != nil {
		return "none"
	}
	return fmt.Sprintf("%04X", n)
}

// arm copies the running kernel to the trial slot, writes the entry that boots it, and sets BootNext.
func arm(disk, esp, trial string) error {
	p, err := partition(disk)
	if err != nil {
		return err
	}
	if err := copyFile(filepath.Join(esp, "EFI/BOOT/BOOTX64.EFI"), trial); err != nil {
		return err
	}
	syscall.Sync()
	entry := efivar.LoadOption("naru trial", p.DevicePath(`\EFI\BOOT\TRIAL.EFI`))
	if err := efivar.WriteVariable(efivar.Dir, efivar.Boot(trialEntry), entry); err != nil {
		return err
	}
	return efivar.SetBootNext(efivar.Dir, trialEntry)
}

// partition reads the first entry of the GPT of disk, where the ESP is.
func partition(disk string) (efivar.ESP, error) {
	f, err := os.Open(disk)
	if err != nil {
		return efivar.ESP{}, err
	}
	defer func() { _ = f.Close() }()
	// The entries start at LBA 2 of a disk the table of which is where sgdisk puts it.
	var e [128]byte
	if _, err := f.ReadAt(e[:], 2*512); err != nil {
		return efivar.ESP{}, err
	}
	p := efivar.ESP{Number: 1, Start: binary.LittleEndian.Uint64(e[32:])}
	p.Sectors = binary.LittleEndian.Uint64(e[40:]) - p.Start + 1
	copy(p.GUID[:], e[16:32])
	return p, nil
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
