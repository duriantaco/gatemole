package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/duriantaco/vouch/internal/kernel/peercred"
	"golang.org/x/sys/unix"
)

const maxConfigSnapshotBytes = 2 << 20

// configFileSnapshot is one inode-stable, bounded view of a configured
// authority file. Parsers and evidence digests must consume Data rather than
// reopening Path.
type configFileSnapshot struct {
	Data   []byte
	Digest string
}

func readConfigFileSnapshot(
	filePath string,
	label string,
	production bool,
) (configFileSnapshot, error) {
	return readConfigFileSnapshotWithOpen(
		filePath,
		label,
		production,
		openConfigSnapshotFile,
	)
}

func openConfigSnapshotFile(filePath string) (*os.File, error) {
	descriptor, err := unix.Open(
		filePath,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK,
		0,
	)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(descriptor), filePath), nil
}

func readConfigFileSnapshotWithOpen(
	filePath string,
	label string,
	production bool,
	openFile func(string) (*os.File, error),
) (configFileSnapshot, error) {
	before, err := os.Lstat(filePath)
	if err != nil {
		return configFileSnapshot{}, fmt.Errorf(
			"vouchd: inspect %s file: %w",
			configLabel(production, label),
			err,
		)
	}
	if err := validateConfigSnapshotInfo(before, label, production); err != nil {
		return configFileSnapshot{}, err
	}

	file, err := openFile(filePath)
	if err != nil {
		return configFileSnapshot{}, fmt.Errorf(
			"vouchd: open %s file: %w",
			configLabel(production, label),
			err,
		)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return configFileSnapshot{}, fmt.Errorf(
			"vouchd: inspect opened %s file: %w",
			configLabel(production, label),
			err,
		)
	}
	if err := validateConfigSnapshotInfo(opened, label, production); err != nil {
		return configFileSnapshot{}, err
	}
	if !os.SameFile(before, opened) {
		return configFileSnapshot{}, fmt.Errorf(
			"vouchd: %s file changed while it was opened",
			configLabel(production, label),
		)
	}

	data, err := io.ReadAll(io.LimitReader(file, maxConfigSnapshotBytes+1))
	if err != nil {
		return configFileSnapshot{}, fmt.Errorf(
			"vouchd: read %s file: %w",
			configLabel(production, label),
			err,
		)
	}
	if len(data) > maxConfigSnapshotBytes {
		return configFileSnapshot{}, fmt.Errorf(
			"vouchd: %s exceeds 2 MiB",
			configLabel(production, label),
		)
	}

	after, err := os.Lstat(filePath)
	if err != nil {
		return configFileSnapshot{}, fmt.Errorf(
			"vouchd: re-inspect %s file: %w",
			configLabel(production, label),
			err,
		)
	}
	if err := validateConfigSnapshotInfo(after, label, production); err != nil {
		return configFileSnapshot{}, err
	}
	if !os.SameFile(opened, after) {
		return configFileSnapshot{}, fmt.Errorf(
			"vouchd: %s file changed while it was read",
			configLabel(production, label),
		)
	}

	sum := sha256.Sum256(data)
	return configFileSnapshot{
		Data:   data,
		Digest: "sha256:" + hex.EncodeToString(sum[:]),
	}, nil
}

func validateConfigSnapshotInfo(
	info os.FileInfo,
	label string,
	production bool,
) error {
	if info == nil || !info.Mode().IsRegular() ||
		info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf(
			"vouchd: %s must be a regular non-symlink file",
			configLabel(production, label),
		)
	}
	if info.Size() < 0 || info.Size() > maxConfigSnapshotBytes {
		return fmt.Errorf(
			"vouchd: %s must be no larger than 2 MiB",
			configLabel(production, label),
		)
	}
	if !production {
		return nil
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf(
			"vouchd: production %s must not be group- or world-writable",
			label,
		)
	}
	ownerUID, err := peercred.FileOwnerUID(info)
	if err != nil {
		return fmt.Errorf(
			"vouchd: inspect production %s owner: %w",
			label,
			err,
		)
	}
	daemonUID := uint32(os.Geteuid())
	if ownerUID != 0 && ownerUID != daemonUID {
		return fmt.Errorf(
			"vouchd: production %s must be owned by root or the daemon user",
			label,
		)
	}
	return nil
}

func configLabel(production bool, label string) string {
	if production {
		return "production " + label
	}
	return label
}

func cloneConfigBytes(data []byte) []byte {
	if data == nil {
		return nil
	}
	return append([]byte(nil), data...)
}
